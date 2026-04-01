<img src="./docs/assets/Conch-logo.jpg" alt="Conch logo" style="width:200px;" />

<a href="https://atomgit.com/openeuler/Conch.git"><img src="https://img.shields.io/badge/atomgit-Conch-blue"/></a> ![license](https://img.shields.io/badge/license-Mulan%20PSL%20v2-blue) <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.23+-blue"/> </a><a href="https://www.python.org/"><img src="https://img.shields.io/badge/Python-SDK-blue"/> </a>

# Conch - Agent Sandbox Engine


Conch 是一个基于 Go 开发的容器沙箱引擎，能够适用于 Agent 对沙箱的高启动性能、高弹性、高 I/O 性能和高密度部署的诉求。
项目围绕以下 Agent 对沙箱新需求展开：
1. 新生态：相比传统命令行和K8S云原生生态，提供 Agent 原生的沙箱管理 API 和 SDK；
2. 新镜像：相对传统 OCI v1 容器镜像格式，提供 EROFS 镜像格式，统一管理容器镜像和快照；
3. 新硬件（超节点）：相比传统单机管理容器镜像，利用超节点高速互联能力，提供跨级镜像共享和管理机制。

## 核心特性

- 轻量安全隔离 -- 支持虚拟沙箱，对 Agent 任务进行安全隔离。支持完整的生命周期管理，包括创建、暂停、恢复和删除等操作。
- 快照启动加速 -- 支持虚拟机内存和根文件系统的快照功能。通过快照机制，可以实现秒级的沙箱启动，显著提升大规模部署场景下的资源利用效率。快照采用写时复制（Copy-on-Write）技术，最小化存储开销。
- 精简容器网络 -- 通过 veth 设备和 NAT 规则实现网络隔离和地址转换，支持容器网络池化复用，降低启动时延。

## 快速开始

### 环境要求

- Go 1.23+
- Containerd 2.2.1+
- Cloud-Hypervisor v48.0+
- Iptables 网络配置工具
- Linux 5.10+

### 一键编译安装


```bash
# 克隆代码仓库
git clone https://atomgit.com/openeuler/Conch.git
cd Conch
git checkout demo

# 一键执行全流程
./scripts/conch-env-setup.sh all

pip install -e ./sdk
```

### 运行服务

编译完成后，二进制文件位于 `bin/` 目录下，通过以下命令启动conchd服务：

```bash
./bin/conchd
```

### conch build 示例

`conch build` 兼容 `buildah bud` 参数，并由 Conch 处理 Dockerfile 中的 `KERNEL` / `SNAP` 扩展指令。

最小 rootfs Dockerfile 示例：

```dockerfile
FROM scratch
COPY hello.txt /hello.txt
```

启用 SNAP 流程时，在 Dockerfile 中补充 `KERNEL` 与 `SNAP`：

```dockerfile
FROM scratch
COPY hello.txt /hello.txt

KERNEL bzImage conch.initrd
SNAP
```

```bash
# 查看帮助
./bin/conch --help
./bin/conch build --help

# 普通构建（仅 rootfs）
./bin/conch build -f Dockerfile -t localhost/demo:latest .

# 启用 SNAP 流程（需 Dockerfile 中包含 KERNEL + SNAP）
CONCH_EROFS_OUTPUT_DIR=/tmp/conch-erofs \
BUILDAH_CONCH_API_URL=http://127.0.0.1:4063 \
./bin/conch build -f Dockerfile.snap -t localhost/demo-snap:latest .
```

启用 `SNAP` 后，命令会额外打印构建出的镜像名与推送示例，例如：

```text
Build outputs:
  RootFS build image: localhost/demo-snap:latest
  PMEM RootFS image:  localhost/conch/pmem-rootfs:latest
  Kernel image:       localhost/conch/kernel:latest
  Sandbox snapshot:   localhost/conch/sandbox-snapshot:latest
  Push command:       buildah manifest push --all localhost/conch/sandbox-snapshot:latest docker://<registry>/<repository>:<tag>
```

### conch unpack 示例

`conch unpack` 用于替代原来的 `conch-unpack`，把 boot OCI index 解包到 containerd 并回写关联的 snapshot label。

支持通过 `-n`/`--namespace` 指定 containerd namespace，默认使用 `default`。

```bash
# 查看帮助
./bin/conch unpack --help

# 解包 boot OCI index（默认 namespace: default）
./bin/conch unpack hub.oepkgs.net/conch/conch-index:v0.1

# 解包到指定 namespace
./bin/conch unpack -n default hub.oepkgs.net/conch/conch-index:v0.1
```

### Python SDK 示例
```python
from conch import Sandbox

try:
    sandbox = Sandbox.create()
    print(f"Sandbox created: {sandbox.sandbox_id}")
    result = sandbox.execute(cmd="python3", content="print('hello Conch!')")
    print(result)
except RuntimeError as e:
    print(f"Error: {e}")
finally:
    sandbox.delete()
```

调用 `execute()` 之前必须先成功执行 `Sandbox.create()` 类方法，并确保 `./bin/conchd` 已经启动；否则 `Sandbox` 实例还没有关联到可用的 Agent client。

## 许可证

木兰宽松许可证， 第2版

## 贡献指南

欢迎社区贡献代码和文档。
