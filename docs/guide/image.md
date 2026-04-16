# Conch Image Guide

本文档介绍 Conch 镜像管理的常用命令，包括构建、发布、拉取和解包。

## 1. conch build

`conch build` 兼容 `buildah bud` 参数，并由 Conch 处理 Dockerfile 中的 `KERNEL` / `INDEX` / `SNAP` 扩展指令。

### 1.1 普通 rootfs 构建

最小 rootfs Dockerfile 示例：

```dockerfile
FROM docker.io/library/busybox:latest
RUN echo "hello Conch" > /hello.txt
```

构建命令：

```bash
conch build -f Dockerfile -t localhost/demo:latest .
```

### 1.2 构建 sandbox-image

生成 `sandbox-image` 时，在 Dockerfile 中补充 `KERNEL` 与 `INDEX`：

```dockerfile
FROM docker.io/library/busybox:latest
RUN echo "hello Conch" > /hello.txt

KERNEL bzImage conch.initrd
INDEX
```

构建命令：

```bash
conch build -f Dockerfile -t localhost/demo-sandbox:latest .
```

启用 `INDEX` 后，命令会先将 rootfs 转换为 PMEM/EROFS rootfs 镜像，再与 kernel 镜像组装为 `sandbox-image`，最终镜像名使用 `-t` 指定的 tag。

### 1.3 构建 sandbox-snapshot

启用 SNAP 流程时，在 Dockerfile 中补充 `KERNEL` 与 `SNAP`：

```dockerfile
FROM docker.io/library/busybox:latest
RUN echo "hello Conch" > /hello.txt

KERNEL bzImage conch.initrd
SNAP
```

构建命令：

```bash
CONCH_EROFS_OUTPUT_DIR=/tmp/conch-erofs \
conch build --config config/config.yaml -f Dockerfile.snap -t localhost/demo-snap:latest .
```

启用 `SNAP` 后，命令会额外打印构建出的镜像名与推送示例，例如：

```text
Build outputs:
  RootFS build image: localhost/demo-snap:latest
  PMEM RootFS image:  localhost/conch/pmem-rootfs:latest
  Kernel image:       localhost/conch/kernel:latest
  Sandbox snapshot:   localhost/conch/sandbox-snapshot:latest
  Push command:       conch push localhost/conch/sandbox-snapshot:latest <registry>/<repository>:<tag>
```

## 2. conch push

`conch push` 用于发布 Conch 镜像产物，包括 `sandbox-image`、`sandbox-snapshot`，以及 kernel multi-arch 镜像。

认证建议通过 `buildah login <registry>` 提前完成。

```bash
# 推送到远端 registry
conch push localhost/demo-sandbox:latest hub.oepkgs.net/conch/demo-sandbox:latest

# 如目标 registry 需要 plain HTTP 或跳过 TLS 校验
conch push --plain-http localhost/demo-sandbox:latest conch.example.com/conch/demo-sandbox:latest

# 推送 kernel multi-arch 镜像
conch push localhost/conch/kernel:6.6.0 hub.oepkgs.net/conch/kernel:6.6.0
```

## 3. conch pull

`conch pull` 用于拉取 Conch 镜像，并在本地自动完成 unpack。

### 3.1 拉取 Conch 原生镜像

```bash
conch pull hub.oepkgs.net/conch/sandbox-snapshot:latest
```

### 3.2 拉取普通 OCI 镜像并转换

对于标准 OCI 镜像，`conch pull` 会将其转换为 Conch 可运行输入后，再在本地完成 unpack。

```bash
conch pull docker.io/library/nginx:latest
```

如源镜像所在 registry 需要，可显式指定源镜像的拉取参数：

```bash
conch pull --plain-http --user <username:password> docker.io/library/nginx:latest
```

如默认 kernel 镜像使用独立 registry，也可单独指定 kernel 镜像的拉取参数：

```bash
conch pull --kernel-plain-http --kernel-user <username:password> docker.io/library/nginx:latest
```

`conch pull` 与 `conch unpack` 均支持通过 config 读取：

- `containerd.socket`
- `containerd.default_namespace`

其中，标准 OCI 镜像转换流程还会使用默认的 kernel 镜像配置。默认配置使用 `hub.oepkgs.net/conch/kernel:6.6.0`，该 tag 应发布为 multi-arch 镜像，由镜像仓库和本地拉取工具自动选择对应架构。

## 4. conch unpack

`conch unpack` 用于把 Conch boot OCI index 解包到 containerd，并回写 rootfs 与 vm/mem snapshot 的关联。

```bash
# 解包 boot OCI index（默认 namespace: default）
conch unpack hub.oepkgs.net/conch/conch-index:v0.1

# 解包到指定 namespace
conch unpack -n default hub.oepkgs.net/conch/conch-index:v0.1
```

## 5. 相关文档

- 镜像工作流设计：`docs/design/image-workflow.md`
- 构建脚本设计：`docs/design/build-conch-images-script.md`
