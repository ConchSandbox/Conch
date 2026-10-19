# 对接 AgentENV Gateway / Scheduler

Conch 可以作为 AgentENV Node，通过原版 Gateway 和 Scheduler 接入官方 E2B SDK。本期支持 create/get/list/delete、checkpoint、pause/connect、timeout/refreshes、update_network，commands/files/PTY 与应用端口代理；SDK 必须显式使用 **non-secure**。Conch 保留 `conch-init` 的 guest 初始化与网络配置，随后等待 envd `/health` 和 `/init` 成功才返回创建结果。

本文的上游基线为 AgentENV commit `1d742e4e149092be895f2c3cf0a097201229a250`。

## 接入形态

两种形态使用同一套 Node API：

```text
单机：E2B SDK -> conchd :8000 -> guest envd

集群：E2B SDK -> Gateway :8080 -> conchd :8000 -> guest envd
                  |           |
                  +-> Scheduler :9090 <-+
```

- **单机**：只运行一个 conchd。SDK 通过 `e2b.listen_addr` 直接访问 Node API，控制面与数据面共用同一监听地址，不部署任何 AgentENV 组件，适合开发与验证，见[第 1 节](#1-单机使用sdk-直连-node-api)。
- **集群**：原版 Gateway 与 Scheduler 调度多个 Conch Node。SDK 统一访问 Gateway，Node API 与 Scheduler 端口仅用于集群内部，见[第 2 节](#2-集群使用)。

## 1. 单机使用（SDK 直连 Node API）

以下 Conch 命令均在仓库根目录执行。

### 1.1 准备依赖并编译

主机需要 KVM、VMM、EROFS 工具、CNI，以及支持 EROFS 的 guest kernel。安装和主机配置方法见[环境准备](environment-setup.md)。

```bash
make build
make build-conch-init-initramfs
```

产物：

- `bin/conchd`、`bin/conch`
- `build-artifacts/conch-init-initramfs.cpio.gz`

### 1.2 配置并启动 Conch

创建本地配置：

```bash
cp config/config.yaml config/config.local.yaml
chmod 0600 config/config.local.yaml
```

把其中 `e2b` 一节替换为以下内容，不要追加重复的 YAML 节。`cluster` 一节保持默认（`scheduler_addr` 为空）即可单独运行；接入集群时再按[第 2 节](#2-集群使用)配置：

```yaml
e2b:
  listen_addr: "127.0.0.1:8000"
  sandbox_proxy_domains: []
  max_sandbox_ttl: 24h
```

`e2b.listen_addr` 为空时禁用 TCP API；启用时必须配置 `e2b.max_sandbox_ttl`（create/connect/set_timeout 共用上限，refreshes 钳制在内，示例为上游 E2B 标准套餐上限 `24h`）。Conch 仅从环境变量 `AENV_API_KEY` 读取密钥，YAML 不提供密钥字段。

同时检查 `sandbox.backend` 和对应 binary 路径。当前默认是 StratoVirt；如果使用已验证的 Cloud Hypervisor，需要修改已有 `sandbox` 节中的对应字段，保留其他配置：

```yaml
sandbox:
  backend: cloud-hypervisor
  cloud_hypervisor:
    binary: /usr/local/bin/cloud-hypervisor
  # 其余已有配置保留
```

生成一次密钥并启动：

```bash
export AENV_API_KEY="e2b_$(openssl rand -hex 32)"

sudo --preserve-env=AENV_API_KEY \
  ./bin/conchd --config config/config.local.yaml
```

在后续终端中设置相同的 `AENV_API_KEY`，不要分别生成不同的密钥。conchd 在前台运行。创建操作的总时限复用 `sandbox.request_timeout`，envd 初始化使用这次创建请求的剩余时间。envd 默认用户和工作目录固定为 `user`、`/home/user`，由预置镜像提供。

### 1.3 准备包含 envd 的模板

普通 Ubuntu/nginx 镜像不够，需要包含 envd、`user` 用户和 `/home/user`。可以构建仓库的 [e2b-rootfs 示例](../../examples/e2b-rootfs/README.md)。该构建示例需要已经运行的 BuildKit 和 registry。

以下假设 rootfs 镜像已经推送到本机 registry：

```text
localhost:5000/conch/e2b-rootfs:debug
```

在设置了相同 `AENV_API_KEY` 的终端创建模板。替换实际 guest kernel 路径；如果镜像使用其他名称，也一并修改 `--source`：

```bash
sudo --preserve-env=AENV_API_KEY ./bin/conch template create \
  --config config/config.local.yaml \
  --name localhost:5000/conch/e2b:demo \
  --source localhost:5000/conch/e2b-rootfs:debug \
  --plain-http \
  --kernel /实际路径/guest-kernel \
  --initrd build-artifacts/conch-init-initramfs.cpio.gz
```

`--plain-http` 对应示例中的明文 registry；HTTPS registry 不需要它。模板创建成功后，再进行 SDK 操作。

### 1.4 运行官方 E2B SDK

SDK 客户端设置三个变量，`E2B_SANDBOX_URL` 与 `E2B_API_URL` 同值，均不加 `/proxy` 后缀：

```bash
python3 -m venv .venv-agentenv
.venv-agentenv/bin/pip install 'e2b==2.46.4'

export E2B_API_KEY="$AENV_API_KEY"
export E2B_API_URL=http://127.0.0.1:8000
export E2B_SANDBOX_URL="$E2B_API_URL"
export E2B_TEMPLATE_ID=localhost:5000/conch/e2b:demo
```

验证一个实例：

```bash
.venv-agentenv/bin/python - <<'PY'
import os
from e2b import Sandbox

box = Sandbox.create(
    os.environ["E2B_TEMPLATE_ID"],
    secure=False,
    timeout=300,
)
try:
    print("sandbox:", box.sandbox_id)
    print(box.commands.run("echo hello-from-conch").stdout)
    box.files.write("/tmp/hello.txt", "Conch + AgentENV")
    print(box.files.read("/tmp/hello.txt"))
finally:
    box.kill()
PY
```

看到命令输出和文件内容，就说明完整链路跑通了。当前必须显式设置 `secure=False`，不要依赖 SDK 的 secure 默认值；`resume`/`fork` 等后续能力返回 `501 Not Implemented`。

### 1.5 运行全量接口测试

[`examples/agentenv/e2b-fullsdk-test-single-node.py`](../../examples/agentenv/e2b-fullsdk-test-single-node.py) 是单机全量接口测试，SDK 直连 Node API，覆盖：

- 控制面全部端点：CRUD、鉴权、分页与过滤、错误码、checkpoint 快照（指定名/自动名/同名更新、`list_snapshots` 分页）、pause/connect/TTL 钳制/update_network；
- envd 数据接口：commands/files/PTY、流式回调、批量上传、watch_dir、upload_url/download_url；
- 数据面代理：Header 与 `/proxy` 前缀路由、HTTP/SSE/WebSocket、`maskRequestHost` 改写、路由错误码；
- 生命周期与 code-interpreter：到期自动删除、`on_timeout=pause` 到期自动暂停、autoResume 数据面唤醒（含并发唤醒）、run_code；
- 收尾自动清理本套件产生的 checkpoint 模板。

安装依赖并沿用前文环境变量（`E2B_SANDBOX_URL` 未设置时脚本自动取 `E2B_API_URL`）：

```bash
.venv-agentenv/bin/pip install 'e2b-code-interpreter' 'websockets==15.0.1'
.venv-agentenv/bin/python examples/agentenv/e2b-fullsdk-test-single-node.py
```

注意事项：

- 模板需包含 Python 3 与 envd（[e2b-rootfs](../../examples/e2b-rootfs/README.md) 构建即可）；code-interpreter 用例需 Jupyter。
- egress 白/黑名单拦截用例需要外网，不可达时自动跳过；数据面不可达时跳过依赖 envd 的用例，控制面用例仍会执行。
- 脚本启动时会列出节点现有沙箱并在非空时警告，最好在空闲节点上运行；它会删除测试沙箱。
- checkpoint 模板清理经本机 conch CLI（unix socket）完成，非默认部署可用 `CONCH_CLI`（如 `"sudo conch"`）与 `CONCH_CLI_CONFIG` 指定。

## 2. 集群使用

### 2.1 节点与配置文件

先按[环境准备](environment-setup.md)和[快速开始](getting-started.md)准备两个可正常启动 VMM 的 Conch 主机、CNI、guest kernel 和 `conch-init` initrd。示例地址如下，请替换为实际地址：

| 角色 | 地址 | 配置 |
| --- | --- | --- |
| Scheduler / Gateway | `10.0.0.10:9090` / `10.0.0.10:8080` | [scheduler.json](../../examples/agentenv/scheduler.json)、[gateway.json](../../examples/agentenv/gateway.json) |
| Conch Node A | `10.0.0.11:8000` | [conch-node-a.yaml](../../examples/agentenv/conch-node-a.yaml) |
| Conch Node B | `10.0.0.12:8000` | [conch-node-b.yaml](../../examples/agentenv/conch-node-b.yaml) |

四个示例配置文件的作用：

- **conch-node-a.yaml / conch-node-b.yaml**：Node 侧 `e2b` / `cluster` 两节的增量配置，两文件除 `node_id` 外完全相同。将这两节**替换**到各自主机已可运行的 Conch 配置中，不要追加重复的 YAML 节。`cluster.node_id` 必须与 Scheduler `nodes[].id` 完全相同，两台 Node 使用同一 `cluster_id`；`cluster.scheduler_addr` 指向 Scheduler gRPC 监听地址，为空时禁用心跳。保存后的配置应设为 `0600` 或 `0640`。
- **scheduler.json**：Scheduler 配置。`nodes` 在 static discovery 下列出全部候选节点；`grpc_listen_addr` 是 gRPC 监听（Node 心跳与 Gateway 查询都走这里）；`strategy` 为调度策略（示例 round_robin）；`report_ttl`/`binding_ttl` 是节点上报与沙箱绑定的内存过期时间；`redis_addr` 为空表示不启用外部存储。
- **gateway.json**：Gateway 配置。`http_listen_addr` 是 SDK 统一入口；`scheduler_addr` 指向 Scheduler gRPC；`request_timeout` 需覆盖创建、恢复等长操作；`sandbox_proxy_domains` 与各 Node 的 `e2b.sandbox_proxy_domains` 保持一致（可为空）。

Gateway 必须能够访问两个 Node 的 TCP API，Node 和 Gateway 都须能连接 Scheduler。在 Gateway 和两台 Node 上设置**同一个** `AENV_API_KEY`，可在一处生成后通过自己的密钥分发方式配置另外两处：

```bash
export AENV_API_KEY="e2b_$(openssl rand -hex 32)"
```

AgentENV Gateway 要求密钥为 32–256 个 URL-safe 字符。使用 sudo 启动 Node 时保留该变量：

```bash
# 在各 Node 的 Conch 仓库根目录执行；B 节点换成自己的配置路径。
chmod 0600 config/conch-node-a.yaml
sudo --preserve-env=AENV_API_KEY ./bin/conchd --config config/conch-node-a.yaml
```

heartbeat 周期与 RPC 超时内部固定为 `5s`。本机容量同时计入启动中和已运行实例，创建前预留，确认释放后归还；总 vCPU 配额取宿主可见逻辑 CPU 数，总内存配额取宿主总内存（MiB），实例数量不设固定上限。普通 E2B 创建请求的资源仍取自 `sandbox.default_spec` 及模板实际规格。

### 2.2 预置相同模板

Scheduler 不保证选中的节点已有模板，因此**开始 SDK 流量前**，所有候选节点都必须有相同 Template Name → digest 映射及工件。本期不提供 E2B 模板管理 API，使用 Conch CLI 完成预置。

先按 [e2b-rootfs 构建说明](../../examples/e2b-rootfs/README.md)构建并推送包含 envd 的 rootfs。该镜像保留 `/etc/conch/entrypoint`，由 `conch-init` 在完成网络初始化后启动；不要将 initrd 替换成 envd。镜像中的 envd 使用 `-isnotfc`，关闭 Firecracker MMDS 和 guest port-forwarder。

在 Node A 的 Conch 仓库根目录创建一次模板，再发布到两台 Node 均可访问的 registry。以下 registry、rootfs 引用和 kernel 路径需换成实际值：

```bash
export E2B_TEMPLATE_ID=registry.example.com/conch/e2b:agentenv-v1
sudo --preserve-env=AENV_API_KEY ./bin/conch template create \
  --config config/conch-node-a.yaml \
  --name "$E2B_TEMPLATE_ID" \
  --source registry.example.com/conch/e2b-rootfs:agentenv-v1 \
  --kernel /path/to/guest/kernel \
  --initrd build-artifacts/conch-init-initramfs.cpio.gz

sudo --preserve-env=AENV_API_KEY ./bin/conch template push \
  --config config/conch-node-a.yaml \
  "$E2B_TEMPLATE_ID" "$E2B_TEMPLATE_ID"
```

在 A、B 各自执行以下命令，B 使用 `config/conch-node-b.yaml`：

```bash
export E2B_TEMPLATE_ID=registry.example.com/conch/e2b:agentenv-v1
sudo --preserve-env=AENV_API_KEY ./bin/conch template pull --config config/conch-node-a.yaml "$E2B_TEMPLATE_ID"
sudo --preserve-env=AENV_API_KEY ./bin/conch template inspect --config config/conch-node-a.yaml "$E2B_TEMPLATE_ID"
```

确认两侧输出的 Template ID 完全相同，再启用 SDK 流量。CLI registry 命令支持 `--plain-http`；仅对实际使用明文的测试 registry 添加此选项，并置于位置参数之前。SDK 的 `templateID` 可以使用上述完整名称，也可以使用已经在两台 Node 存在的不可变 `sha256:...` digest；SDK 默认的 `base` 不会自动映射到此模板。

冷启动（cold）模板的 CPU/内存取自 Node 的 `sandbox.default_spec`。从 checkpoint 恢复的 resume 模板按捕获时的实际 CPU/内存进行容量校验与预留；当前 Conch 生成的 checkpoint 会保存 CPU 元数据 `io.conch.cpu-count`。E2B `/resume` 接口仍返回 `501`。

### 2.3 构建和启动原版 AgentENV

在控制面主机编译固定版本，无需更改 AgentENV 源码、重新生成 proto 或构建其 Rust Node：

```bash
git clone https://github.com/kvcache-ai/AgentENV.git AgentENV
git -C AgentENV checkout --detach 1d742e4e149092be895f2c3cf0a097201229a250
make -C AgentENV/services build
```

先把 [scheduler.json](../../examples/agentenv/scheduler.json)、[gateway.json](../../examples/agentenv/gateway.json) 中的地址替换为实际部署地址，再在分别运行 Scheduler、Gateway 的终端中执行：

```bash
./AgentENV/services/bin/scheduler -config /path/to/Conch/examples/agentenv/scheduler.json
```

```bash
# 此终端已配置与两个 Node 相同的 AENV_API_KEY。
./AgentENV/services/bin/gateway -config /path/to/Conch/examples/agentenv/gateway.json
```

检查 Gateway 可达以及两个节点的观测状态：

```bash
export E2B_API_KEY="$AENV_API_KEY"
export E2B_API_URL=http://10.0.0.10:8080
curl -fsS "$E2B_API_URL/health"
curl -fsS -H "X-API-Key: $E2B_API_KEY" \
  "$E2B_API_URL/nodes?clusterID=conch-agentenv-demo"
```

节点探活只打不带路由信息的 `GET /health`，不要加 `E2b-Sandbox-Id`、`E2b-Sandbox-Port` 或沙箱 `Host`。带了这些头的 `/health` 会进该沙箱的 envd，返回的是 guest 状态，不是节点状态。

`/nodes` 应显示两个 Node、`ready` 状态及实际分配资源。示例使用 static discovery、round-robin 和内存 binding，未配置 Scheduler 的 `node_resource_limit`，因此没有启用其资源阈值过滤。本机请求容量校验仍由 Conch 执行。

### 2.4 运行官方 SDK

SDK 客户端设置三个变量；**两个 URL 都指向 Gateway，且不加 `/proxy` 后缀**：

```bash
export E2B_API_KEY="$AENV_API_KEY"
export E2B_API_URL=http://10.0.0.10:8080
export E2B_SANDBOX_URL="$E2B_API_URL"
export E2B_TEMPLATE_ID=registry.example.com/conch/e2b:agentenv-v1
```

在 Conch 仓库根目录执行：

```bash
.venv-agentenv/bin/python - <<'PY'
import os
from e2b import Sandbox

box = Sandbox.create(
    os.environ["E2B_TEMPLATE_ID"],
    secure=False,
    timeout=300,
)
try:
    print("sandbox:", box.sandbox_id)
    print(box.commands.run("echo hello-from-conch").stdout)
finally:
    box.kill()
PY
```

在两个正常节点且无其他创建流量的条件下，round-robin 会把连续的创建分配到不同 Node；可结合 Node 日志中的 sandbox ID 确认回路由。创建时的 `timeout` 决定实例初始存活时间：SDK 缺省 `300` 秒，到期自动删除；HTTP 创建请求显式传 `0` 或负数返回 `400`，上限为 `e2b.max_sandbox_ttl`。它与 Node 创建操作的 `sandbox.request_timeout` 不同，到期时间可通过 set_timeout / connect / refreshes 更新。

没有多台物理机时，也可以在一台宿主机上模拟双节点集群：Scheduler 和 Gateway 运行在宿主机，两个 `--privileged` 的 Docker 容器内各运行一个 conchd，把不同宿主端口映射到容器的 `:8000`，scheduler.json 的 `nodes` 改指宿主机映射地址。容器内需安装 StratoVirt 与 erofs-utils，两个节点使用不同 CID 起始值构建的 conchd，其余配置与物理双节点一致。

## 3. Header 和 Host 路由

默认的 `sandbox_proxy_domains: []` 配合 `E2B_SANDBOX_URL` 即可运行 commands/files。SDK 数据请求携带 `E2b-Sandbox-Id` 和 `E2b-Sandbox-Port`；集群形态下 Gateway 查询该实例绑定，将 `/proxy` 流量交给拥有它的 Conch，单机直连时由 conchd 内嵌的代理直接处理，再访问 guest envd。这两个请求是 guest envd 探活，不是上一节的节点探活。以下请求使用一个仍存活的 sandbox ID：

```bash
export SANDBOX_ID='替换为仍存活的实例ID'
curl -i -H "X-API-Key: $E2B_API_KEY" \
  -H "E2b-Sandbox-Id: $SANDBOX_ID" \
  -H "E2b-Sandbox-Port: 49983" \
  "$E2B_API_URL/health"
```

要使用 `get_host()` / `getHost()` 生成的应用 URL，在 Gateway 和两个 Conch 的 `sandbox_proxy_domains` 中配置相同裸域，例如 `sandboxes.example.com`。域名不包含 scheme、端口或路径，须将 `*.sandboxes.example.com` 解析到 Gateway；Conch 返回第一个配置域。应用 URL 形如 `{port}-{sandboxID}.sandboxes.example.com`，guest 应用监听 `0.0.0.0:{port}`。

若 Gateway 仍使用示例端口 `8080`，浏览器/curl URL 需带 `:8080`；SDK `get_host()` 返回的主机名不会包含这个 Gateway 端口。无需 DNS 也可以先通过 Host header 验证路由：

```bash
curl -i -H "Host: 49983-$SANDBOX_ID.sandboxes.example.com" \
  "$E2B_API_URL/health"
```

non-secure 模式的应用/guest 数据流量允许公开访问；控制面仍要求 API key。这里的 `secure=false` 指 envd token 模式，和 Gateway 是否部署 HTTPS 是不同配置。

创建请求可携带 `maskRequestHost`（`network.maskRequestHost`，形如 `example.com:443`）：数据面代理将 `Host` 头替换为该掩码，隐藏沙箱域名，并将原 Host 透传给 guest。未设置时保持原样。

数据面路由行为（Header/`/proxy`/Host 域名路由、`maskRequestHost` 改写、SSE/WebSocket、并发唤醒）由 `examples/agentenv/e2b-fullsdk-test-single-node.py` 的数据面与生命周期用例覆盖；其中 Host 域名路由用例仅当节点配置了 `sandbox_proxy_domains` 时执行。
