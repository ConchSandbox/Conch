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
  --initrd /var/lib/conch/conch.initrd \
  -t localhost/conch/openeuler:latest
```

示例：

```console
# 列出所有 Template
$ conch template ls
ID                             ORIGIN      BOOT_MODE  BOOT_INDEX_DIGEST  SOURCE_SANDBOX  BUILD_REF
tmpl_ab2345da0a69b4e18aa24ad6  image       cold       sha256:1111...     -               localhost/conch/openeuler:latest

# 查看指定 Template
$ conch template inspect tmpl_ab2345da0a69b4e18aa24ad6
ID                             ORIGIN  BOOT_MODE  BOOT_INDEX_DIGEST  SOURCE_SANDBOX  BUILD_REF
tmpl_ab2345da0a69b4e18aa24ad6  image   cold       sha256:1111...     -               localhost/conch/openeuler:latest

# 删除指定 Template
$ conch template rm tmpl_ab2345da0a69b4e18aa24ad6
Removed template: tmpl_ab2345da0a69b4e18aa24ad6
```

## 2. Template 分发

`conch template push / pull` 用于向镜像仓库发布 Template，或从镜像仓库拉取 Template。拉取时会校验 Boot Index、创建本地 Template 并返回新的 Template ID。

示例：

```console
# 将 Template 发布到镜像仓库
$ conch template push tmpl_ab2345da0a69b4e18aa24ad6 registry.example.com/conch/openeuler:latest
Pushed template: tmpl_ab2345da0a69b4e18aa24ad6 -> registry.example.com/conch/openeuler:latest

# 从镜像仓库拉取 Template
$ conch template pull registry.example.com/conch/openeuler:latest
Template: tmpl_c35e71ba26e24b6a92eca151
Boot image: registry.example.com/conch/openeuler:latest
Image digest: sha256:2222...
```

## 3. Image 管理

`conch image pull / push / unpack / ls / rm` 用于管理 conchd 中的 OCI 镜像，其行为与 `ctr` 中对应的镜像操作一致。

> **注意：** 不要使用 `conch image pull` 拉取 Boot Index，请使用 `conch template pull`。
