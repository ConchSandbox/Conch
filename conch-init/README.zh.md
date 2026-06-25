# conch-init

`conch-init` 是 Conch sandbox 内的 guest 侧 init 与控制服务。它以 Rust musl 静态二进制形式构建，被打包进 sandbox initramfs，并在 guest 内作为 PID 1 启动。

不同于 host 侧的 `conchd` 和运行时管理组件，`conch-init` 运行在 sandbox 边界内部。它负责准备 guest 环境、启动 gRPC 控制服务、通过 vsock 完成 sandbox ready 握手，并执行 host SDK 或 runtime 发起的进程与文件操作。

## 系统角色

```text
Host
┌─────────────────────────────────────────────┐
│  conchd / SDK / runtime manager             │
│       │                                     │
│       │  gRPC over sandbox network :4064    │
│       │  vsock ready handshake :4065        │
└───────┼─────────────────────────────────────┘
        │
   Sandbox guest boundary
        │
┌───────▼─────────────────────────────────────┐
│  conch-init  (PID 1 / /init)                │
│       │                                     │
│       ├─ mount proc/sys/dev/tmp/overlay     │
│       ├─ setup guest network                │
│       ├─ start AgentService gRPC server     │
│       ├─ receive sandbox token over vsock   │
│       └─ execute commands and file streams  │
└─────────────────────────────────────────────┘
```

sandbox 启动后，`conch-init` 会立即运行并完成以下工作：

1. **初始化 guest 环境**：挂载基础文件系统，准备 `/dev/null`，挂载存储设备，准备 `/mnt/conch/merge` 下的 OverlayFS，并配置网络。
2. **启动控制面**：在 `0.0.0.0:4064` 上通过 gRPC 暴露 `pb.AgentService`。
3. **处理 vsock ready 握手**：监听 vsock 端口 `4065`，接收 `SANDBOX_ID` 和 `AGENT_TOKEN`，初始化请求认证信息，并在 guest 健康后返回 `READY:<version>`。
4. **运行 rootfs entrypoint hook**：如果 `/mnt/conch/merge/etc/conch/entrypoint` 存在且可执行，则在合并后的 rootfs 内启动它，并等待该 entrypoint 上报 ready。如果 entrypoint 不存在，则不要求 rootfs services ready。
5. **执行 workload 命令**：支持命令执行、内联脚本内容、cwd/env 传递、stdout/stderr 捕获和 exit code 返回。
6. **传输文件**：通过 gRPC 支持流式上传和下载文件。
7. **回收子进程**：作为 PID 1 回收已退出的子进程。

## 目录结构

```text
conch-init/
├── Cargo.toml          # Rust 包元数据和依赖
├── Makefile            # musl build/check/test/install 目标
├── build.rs            # tonic protobuf binding 生成
└── src/
    ├── main.rs         # CLI 模式、PID 1 检测、服务启动
    ├── init.rs         # PID 1 启动流程
    ├── grpc.rs         # AgentService 实现
    ├── vsock.rs        # sandbox-ready 握手
    ├── auth.rs         # gRPC token 认证
    ├── state.rs        # sandbox ready/auth 共享状态
    ├── mount.rs        # guest 文件系统和存储挂载
    ├── network.rs      # guest 网络配置
    ├── devpts.rs       # 合并 rootfs 下的 devpts 配置
    ├── rootfs_entrypoint.rs
    ├── reaper.rs       # PID 1 子进程回收
    ├── logging.rs
    ├── util.rs
    └── pb.rs           # generated protobuf module include
```

相关文件：

```text
api/agent.proto                         # gRPC 协议定义
scripts/build-conch-init-initramfs.sh   # initramfs 构建脚本
Makefile                                # 顶层构建目标
examples/e2b-rootfs/conch-entrypoint.sh # 可选 rootfs 服务 ready hook
```

## 启动流程

`conch-init` 设计上作为 sandbox guest 内的 init 进程运行。Rust 入口始终执行 init 流程；`--init` 仅作为兼容参数保留。

init 流程的执行顺序如下：

1. 设置 `PATH` 为 `/sbin:/bin:/usr/sbin:/usr/bin`。
2. 按需挂载 `/proc`。
3. 从 kernel cmdline 读取 `conch.sandbox_id`。
4. 创建 `/dev/null`。
5. 挂载基础文件系统。
6. 挂载存储设备并准备合并 rootfs。
7. 配置 guest 网络。
8. 如果存在 rootfs entrypoint，则启动它。
9. 当 overlay rootfs 就绪后，`chroot` 到 `/mnt/conch/merge`。
10. 在端口 `4064` 启动 gRPC server。
11. 在 vsock 端口 `4065` 启动 readiness listener。
12. 启动子进程回收逻辑，并等待 shutdown signal。

## Readiness 握手

host 侧通过连接 vsock 端口 `4065` 初始化 sandbox，并发送：

```text
I AM SANDBOX_ID:<sandbox-id>
AGENT_TOKEN:<token>
```

`conch-init` 会保存 token，用于后续 gRPC 请求认证。健康检查通过后返回：

```text
OK
READY:0.0.4
```

如果 gRPC 未就绪，或者已存在的 rootfs entrypoint 在 readiness 超时时间内没有上报 ready，则返回：

```text
NOT_READY
```

当前版本由 `conch-init/src/constants.rs` 中的 `SERVER_VERSION` 定义。

## gRPC API

`conch-init` 实现了 `api/agent.proto` 中定义的 `pb.AgentService`。

| RPC | 作用 |
| --- | --- |
| `HealthCheck(Empty)` | 服务可访问时返回 `message: "OK"`。 |
| `StartProcess(StartProcessRequest)` | 在 guest 内执行命令，并返回 stdout、stderr、exit code 和 error。 |
| `PostFileStream(stream FileChunk)` | 通过 client-streaming RPC 上传单个文件。 |
| `GetFileStream(GetFileRequest)` | 通过 server-streaming RPC 下载单个文件。 |

普通 gRPC 请求都需要携带 metadata：

```text
x-conch-agent-token: <token>
```

metadata 名称继续保留 `x-conch-agent-token`，这是为了协议兼容。

### StartProcess

`StartProcess` 支持：

- `cmd`：可执行文件名或路径。
- `args`：命令参数。
- `cwd`：工作目录；如果不存在会自动创建。
- `env`：环境变量。
- `content`：内联脚本内容。

当设置了 `content` 且 `args` 为空时，`conch-init` 会把内容写入 `cwd` 下的临时文件，并将该文件作为唯一参数传给 `cmd`。脚本后缀会根据命令推断，例如 `python3` 对应 `.py`，`sh` 对应 `.sh`。

示例：

```json
{
  "cmd": "python3",
  "cwd": "/tmp",
  "env": {
    "CONCH_VERIFY_ENV": "ok"
  },
  "content": "import os\nprint(os.environ['CONCH_VERIFY_ENV'])\n"
}
```

### 文件流

`PostFileStream` 接收一组 `FileChunk` 流。第一个 chunk 必须包含 `filepath`，后续 chunk 可以省略。服务端会先写入临时文件，stream 完成后再通过 `rename` 提交到目标路径。

`GetFileStream` 会以 chunk 形式返回文件内容。第一个响应 chunk 包含 `filepath`，后续 chunk 可以为空。

## 构建

`conch-init` 使用 musl target 构建，以便运行在最小 initramfs 环境中。

```bash
make build-conch-init
```

构建产物：

```text
bin/conch-init
conch-init/target/<arch>-unknown-linux-musl/release/conch-init
```

支持的 host 架构：

| Host arch | Rust target |
| --- | --- |
| `x86_64` | `x86_64-unknown-linux-musl` |
| `aarch64` | `aarch64-unknown-linux-musl` |

## 构建 initramfs

将 `conch-init` 打包进 Alpine-based initramfs：

```bash
make build-conch-init-initramfs
```

默认输出：

```text
build-artifacts/conch-init-initramfs.cpio.gz
```

构建脚本会将二进制安装为：

```text
/sbin/conch-init
/init -> /sbin/conch-init
```

常用覆盖参数：

```bash
INIT_BIN=./bin/conch-init \
OUTPUT=./build-artifacts/conch-init-initramfs.cpio.gz \
ALPINE_VERSION=3.20.3 \
./scripts/build-conch-init-initramfs.sh
```

`--agent-bin` 仍可作为 `--init-bin` 的废弃别名使用。

## 测试

运行 Rust 检查和单元测试：

```bash
make -C conch-init check
make -C conch-init test
```

针对运行中的 sandbox 执行 gRPC 验证：

```bash
AGENT_GRPC_ADDR=<sandbox-ip>:4064 \
TEST_AGENT_TOKEN=<token> \
/home/conch-agent/test/verify-agent-grpc.sh
```

验证脚本应覆盖 `api/agent.proto` 中的全部 RPC：健康检查、进程执行、文件上传和文件下载。

## 运维说明

- `conch-init` 日志目录为 `/var/log/conch-init/`。
- rootfs service hook 路径为合并 rootfs 内的 `/etc/conch/entrypoint`。
- 只有合并 rootfs 内存在 `/etc/conch/entrypoint` 时，才要求 rootfs services ready。该 entrypoint 通过创建 `/run/conch/services-ready` 并向 PID 1 发送 `SIGUSR1` 标记 ready。
- gRPC 协议仍保留 `AgentService` 命名，即使组件二进制已经改名为 `conch-init`。
- sandbox-ready 协议仍使用 `AGENT_TOKEN` 字段和 `x-conch-agent-token` metadata，以保持兼容。
