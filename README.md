<img src="./docs/assets/Conch-logo.jpg" alt="Conch logo" style="width:200px;" />

<a href="https://atomgit.com/openeuler/Conch.git"><img src="https://img.shields.io/badge/atomgit-Conch-blue"/></a> ![license](https://img.shields.io/badge/license-Mulan%20PSL%20v2-blue) <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26+-blue"/> </a><a href="https://www.python.org/"><img src="https://img.shields.io/badge/Python-SDK-blue"/> </a>

# Conch - Agent Sandbox Engine


Conch 是一个基于 Go 开发的容器沙箱引擎，能够适用于 Agent 对沙箱的高启动性能、高弹性、高 I/O 性能和高密度部署的诉求。
项目围绕以下 Agent 对沙箱新需求展开：
1. 新生态：相比传统命令行和K8S云原生生态，提供 Agent 原生的沙箱管理 API 和 SDK；
2. 新镜像：相对传统 OCI v1 容器镜像格式，提供 EROFS 镜像格式，统一管理容器镜像和快照；
3. 新硬件（超节点）：相比传统单机管理容器镜像，利用超节点高速互联能力，提供跨级镜像共享和管理机制。

## 核心特性

- 轻量安全隔离 -- 支持虚拟沙箱，对 Agent 任务进行安全隔离。支持完整的生命周期管理，包括创建、暂停、恢复和删除等操作。
- 快照启动加速 -- 支持虚拟机内存和根文件系统的快照功能。通过快照机制，可以实现秒级的沙箱启动，显著提升大规模部署场景下的资源利用效率。快照采用写时复制（Copy-on-Write）技术，最小化存储开销。
- 精简容器网络 -- 通过 CNI 插件管理沙箱网络命名空间的外层网络，同时由 Conch 保留可复用 slot、netns 生命周期、VM guest tap 和 guest NAT 的管理权，在保持网络池化低时延的同时明确外层网络边界。

## 快速开始

### 环境要求

- Go 1.26+
- Cloud-Hypervisor v51.0+
- erofs-utils 1.9+（需要 `mkfs.erofs --fsalignblks`）
- Linux 5.10+
- root 权限，或等价的 `CAP_SYS_ADMIN` 与 `CAP_NET_ADMIN` 能力，用于 network namespace、tap 设备、路由和 NAT 规则
- 主机已安装 CNI 插件二进制文件，默认路径为 `/opt/cni/bin`；默认 bridge 模式至少需要 `bridge`、`host-local` 和 `loopback`
- 存在至少一个 Conch CNI `.conf` 或 `.conflist` 配置文件。默认路径为 `/etc/conch/cni/net.d`；如果该默认路径没有配置，Conch 会回退到已加载配置文件旁边的 `cni/net.d`，例如 `config/cni/net.d`。
- CNI 子网不能与主机网络、集群网络或 VM guest tap 子网重叠
- Iptables 网络配置工具。Conch 仍会为 VM guest tap 路径配置 namespace 内 NAT；所选 CNI 插件也可能依赖 iptables 或 nftables。

网络设计细节与验证步骤见 [Conch Network Guide](docs/guide/net_usage.md)。

Conchd 会在进程内初始化 containerd 服务和 Conch 插件，不需要单独启动系统 `containerd` 守护进程。

### 一键编译安装


```bash
# 克隆代码仓库
git clone https://atomgit.com/openeuler/Conch.git
cd Conch
git checkout demo

# 安装运行依赖、构建二进制并安装 SDK
./scripts/conch-env-setup.sh install
```

### 运行服务

编译完成后，二进制文件位于 `bin/` 目录下，通过以下命令启动conchd服务：

```bash
./bin/conchd
```

### 镜像管理

Conch 提供统一的镜像管理命令，用于将已有 OCI rootfs 镜像转换为可启动 Template，并支持发布、拉取和解包：

```bash
conch template create --source docker.io/library/nginx:latest \
  --kernel ./bzImage \
  --initrd ./conch.initrd \
  -t localhost/conch/nginx:latest

# registry.example.com 是占位域名，请替换为实际镜像仓库
conch image push localhost/conch/nginx:latest registry.example.com/conch/nginx:latest
conch image pull registry.example.com/conch/nginx:latest
# 仅下载 OCI content，不生成 snapshot
conch image pull --skip-unpack registry.example.com/conch/nginx:latest

# 本地已有 Conch 镜像时可单独解包
conch image unpack registry.example.com/conch/nginx:latest
```

其中 `conch template create` 会将标准 OCI rootfs 镜像转换为 native EROFS rootfs，并与 kernel/initrd 组件组装为 Conch boot index，同时注册为 Template；`conch image pull` 默认会在拉取后自动完成本地 unpack，`--skip-unpack` 可只下载 OCI content；`conch image unpack` 主要用于本地已有 Conch 镜像时单独解包或排障。

详细设计见 [Conch Image Workflow Design](docs/design/image-workflow.md)。

### SDK 配置

SDK 需要通过配置文件指定 conchd 的连接方式和沙箱参数。项目提供了默认配置模板 `config/sdk-config.yaml`：

```yaml
sandbox:
  unix_socket: "/var/run/conchd/conchd.sock"   # 优先使用 Unix Socket 连接
  api_url: "http://localhost:4063"              # unix_socket 为空时使用 HTTP 连接
  sandbox_id: ""                                # 留空则自动生成

image:
  vmm_name: "cloud-hypervisor"                  # 虚拟机监视器名称
  vcpu_num: 1                                   # 虚拟 CPU 数量
  ram_mb: 1024                                  # 内存大小（MB）
```

安装 SDK 后可通过命令初始化系统级配置（可选）：

```bash
sudo conch-sdk-init-config        # 复制模板到 /etc/conch/sdk-config.yaml
sudo conch-sdk-init-config -f     # 强制覆盖已有配置
```

配置文件加载优先级（由高到低）：

| 优先级 | 方式 | 说明 |
|--------|------|------|
| 1 | `$CONCH_SDK_CONFIG` 环境变量 | 环境变量指定路径 |
| 2 | `~/.config/conch/sdk-config.yaml` | 用户级配置 |
| 3 | `/etc/conch/sdk-config.yaml` | 系统级配置 |
| 4 | `<repo>/config/sdk-config.yaml` | 仓库内置模板 |

### Python SDK 示例

先将已就绪的 Template ID 写入环境变量：

```bash
export CONCH_TEMPLATE_ID="tmpl_xxx"
```

```python
import os

from conch import Sandbox

sandbox = None
try:
    template_id = os.environ.get("CONCH_TEMPLATE_ID")
    if not template_id:
        raise RuntimeError("CONCH_TEMPLATE_ID is required")

    sandbox = Sandbox.create(template_id=template_id)
    print(f"Sandbox created: {sandbox.sandbox_id}")

    # 获取沙箱信息
    sandbox.get_info()

    # 执行 Python 脚本
    result = sandbox.commands.run(cmd="python3", content="print('hello Conch!')")
    print(result)

    # 执行带参数的系统命令
    result = sandbox.commands.run(cmd="ls", args=["-l", "/root"])
    print(result)
except RuntimeError as e:
    print(f"Error: {e}")
finally:
    if sandbox:
        sandbox.delete()
```

调用 `commands.run()` 之前必须先成功执行 `Sandbox.create()` 类方法，并确保 `./bin/conchd` 已经启动；否则 `Sandbox` 实例还没有关联到可用的 Agent client。

更多 SDK 用法详见 [Python SDK API 文档](docs/guide/python-api.md)。

## 许可证

木兰宽松许可证， 第2版

## 贡献指南

欢迎社区贡献代码和文档。
