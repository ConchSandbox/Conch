# Conch Template 与镜像指南

本文档介绍当前 Conch Template 和镜像内容管理的常用命令。

Image 是传统 OCI 镜像，主要承载 rootfs 和应用内容，可以作为创建 Template 的输入。使用 `conch image` 管理。

Template 是 Conch 中创建 Sandbox 使用的模板，使用 `conch template` 管理。实现上，每个 Template 与一个 Boot Index 一一对应。Boot Index 是符合 OCI 规范的 [Index](https://github.com/opencontainers/image-spec/blob/main/image-index.md)，包含以下组件：

- rootfs manifest：使用 [EROFS](https://github.com/containerd/containerd/blob/main/docs/snapshotters/erofs.md) 替代传统的 tar（.gz）作为镜像层格式。
- sandbox manifest：承载 kernel 和 initrd。
- mem-snapshot manifest：可选，用于恢复启动。

这三个组件均为符合 OCI 规范的 [Manifest](https://github.com/opencontainers/image-spec/blob/main/manifest.md)。

## 1. Template 创建

`conch template create` 用于从已有 OCI rootfs 镜像以及本地 kernel/initrd 文件生成可启动 Template，并返回 Template ID。示例命令如下：

```bash
conch template create \
  --source docker.io/openeuler/openeuler:24.03-lts-sp2 \
  --kernel /var/lib/conch/kernel \
  --initrd /var/lib/conch/conch.initrd
```

### CLI API 请求超时

所有通过 `conch` CLI 调用 conchd API 的请求共用 `CONCH_API_TIMEOUT` 环境变量。未设置时默认超时为 2 分钟；设置为正的 Go duration（例如 `30m`）后，当前命令中的所有 CLI 到 conchd API 请求都会使用该值。这个设置适用于 `template create`、`template pull`、`template push` 及其他使用 conchd API 的 CLI 命令。

例如，首次拉取或转换较大镜像时可执行：

```bash
CONCH_API_TIMEOUT=30m conch template create \
  --source hub.oepkgs.net/openeuler/python:latest \
  --kernel /var/lib/conch/kernel \
  --initrd /var/lib/conch/conch.initrd
```

`conch template push --timeout <duration>` 是 push 命令的局部覆盖，优先于 `CONCH_API_TIMEOUT`；它仅影响该次 push 请求。

创建、拉取和 checkpoint 都会建立由 digest 唯一派生的内部 canonical image record，例如
`localhost/conch/template:sha256-1111...`。这个 record 是 containerd GC 的引用根，不需要用户命名。
该本地命名空间由 Template 生命周期独占，pull 操作不允许把它作为远端输入，普通 `conch image rm` 也不能删除 canonical record。

示例：

```console
# 列出所有 Template
$ conch template ls
TEMPLATE_ID  ORIGIN  BOOT_MODE  SOURCE_REF  SOURCE_SANDBOX  BUILD_REF
sha256:1111...     image   cold       -           -               localhost/conch/template:sha256-1111...

# 查看指定 Template
$ conch template inspect sha256:1111...
TEMPLATE_ID  ORIGIN  BOOT_MODE  SOURCE_REF  SOURCE_SANDBOX  BUILD_REF
sha256:1111...     image   cold       -           -               localhost/conch/template:sha256-1111...

# 删除指定 Template
$ conch template rm sha256:1111...
Removed template: sha256:1111...
```

删除时会移除 Template metadata 和对应的 canonical image record；实际 content 由 containerd GC 在不再被其他记录引用后回收。

## 2. Template 分发

`conch template push / pull` 用于向镜像仓库发布 Template，或从镜像仓库拉取 Template。`pull` 的输入是用户熟悉的远端 registry reference；拉取后会校验 Boot Index，并返回 Template ID 和本地 canonical build ref。`push` 则使用 Template ID 选择本地 Template，再指定远端目标 reference。

示例：

```console
# 将 Template 发布到镜像仓库
$ conch template push sha256:1111... registry.example.com/conch/openeuler:latest
Pushed template: sha256:1111... -> registry.example.com/conch/openeuler:latest

# 从镜像仓库拉取 Template
$ conch template pull registry.example.com/conch/openeuler:latest
Boot image: localhost/conch/template:sha256-2222...
Template ID: sha256:2222...
```

## 3. Image 管理

`conch image pull / push / ls / rm` 用于管理 conchd 中的 OCI 镜像内容，Conch 不会解包普通 OCI 镜像。需要手动解包 Template 的 Boot Index 时，使用 `conch template unpack <template-id>`。

> **注意：** `conch image pull` 会拒绝 Boot Index；请使用 `conch template pull` 拉取并创建对应的 Template。
