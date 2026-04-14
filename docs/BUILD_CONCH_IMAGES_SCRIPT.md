# `build-conch-images.sh` 使用说明

本文档说明 [build-conch-images.sh](/root/Conch/scripts/build-conch-images.sh) 的用途、默认文件位置和常见使用方式。

## 1. 脚本作用

该脚本用于构建或打包 Conch 镜像，支持多种复用场景，不必每次都从 Dockerfile 全量开始。

它最终会生成 3 类镜像：

- VM 镜像：包含 `bzImage` 和 `conch.initrd`
- RootFS 镜像：包含 `.erofs` 层
- Index 镜像：把 VM 和 RootFS 组装成最终清单镜像

默认最终 index 镜像名为：

```bash
<registry>/conch-claw:<tag>
```

如果以后要改默认 index 名，不必改脚本代码，可以直接传：

```bash
--index-repo <new-repo>
```

## 2. 默认文件位置

脚本默认按仓库根目录推导以下路径：

- 仓库根目录：`/root/Conch`
- Dockerfile：`/root/Conch/Dockerfile`
- kernel：`/root/Conch/build-artifacts/bzImage`
- initrd：`/root/Conch/build-artifacts/conch.initrd`
- rootfs tar：`/root/Conch/build-artifacts/conch-rootfs.tar`
- erofs 层：`/root/Conch/build-artifacts/rootfs.erofs` 或 `layer*.erofs`

如果你的文件不在这些位置，可以通过以下参数覆盖：

- `--build-dir`
- `--context-dir`
- `--dockerfile`
- `--input-tar`

## 3. 模式说明

### `full`

从 Dockerfile 开始执行完整流程：

1. 用 `buildah bud` 构建 rootfs 容器镜像
2. 导出为 tar
3. 转成 `.erofs`
4. 构建 VM 镜像
5. 构建 RootFS 镜像
6. 创建最终 index

适合首次全量构建。

### `erofs-only`

只把现有 rootfs tar/docker-archive 转成 `.erofs`，不生成最终镜像。

适合只准备 EROFS 层。

### `pack`

如果目录里已经有 `.erofs`、`bzImage`、`conch.initrd`，直接打包出 VM / RootFS / Index。

适合“已有 EROFS 层，不想再从 Dockerfile 全量开始”的场景。

### `vm-only`

只更新 VM 镜像和最终 index，复用已有 RootFS 镜像。

适合“只改了 `bzImage` / `conch.initrd`”。

### `rootfs-only`

只更新 RootFS 镜像和最终 index，复用已有 VM 镜像。

适合“只改了 `.erofs` 层”。

## 4. 常见命令

### 4.1 首次全量构建

```bash
bash scripts/build-conch-images.sh --mode full --tag dev
```

### 4.2 先预演，不真正执行

```bash
bash scripts/build-conch-images.sh --mode full --dry-run --tag dev
```

### 4.3 已有 `.erofs`，直接打包

```bash
bash scripts/build-conch-images.sh --mode pack --build-dir ./build-artifacts --tag dev
```

### 4.4 只更新 `bzImage` / `conch.initrd`

```bash
bash scripts/build-conch-images.sh --mode vm-only --build-dir ./build-artifacts --tag dev
```

### 4.5 只把 tar 转成 EROFS

```bash
bash scripts/build-conch-images.sh --mode erofs-only --input-tar ./build-artifacts/conch-rootfs.tar
```

### 4.6 修改最终 index 镜像名

```bash
bash scripts/build-conch-images.sh \
  --mode pack \
  --tag dev \
  --index-repo hub.oepkgs.net/conch/my-index
```

## 5. `--dry-run` 行为

`--dry-run` 只打印将执行的步骤和关键命令，不真正调用：

- `buildah`
- `mkfs.erofs`
- `truncate`

但它仍然会保留一部分输入校验，主要用于：

- 快速回忆脚本该怎么用
- 确认默认文件位置是否符合预期
- 预览不同模式会执行哪些步骤

建议在长时间没用这个脚本时，先执行：

```bash
bash scripts/build-conch-images.sh --help
bash scripts/build-conch-images.sh --mode full --dry-run --tag dev
```

## 6. 运行前检查

### `full`

要求：

- `Dockerfile` 可访问
- `bzImage` 存在
- `conch.initrd` 存在
- 安装 `buildah`、`mkfs.erofs`、`jq`、`xz`

### `pack`

要求：

- 目录中已有 `.erofs`
- `bzImage` 存在
- `conch.initrd` 存在
- 安装 `buildah`

### `vm-only`

要求：

- `bzImage` 存在
- `conch.initrd` 存在
- 本地已有 RootFS 镜像

### `rootfs-only`

要求：

- 目录中已有 `.erofs`
- 本地已有 VM 镜像

## 7. 建议使用方式

如果你现在不确定该用哪个模式，可以按下面的顺序判断：

1. 如果只有 Dockerfile、kernel、initrd，选 `full`
2. 如果已经有 `.erofs`，选 `pack`
3. 如果只改了 `bzImage` / `conch.initrd`，选 `vm-only`
4. 如果只改了 `.erofs`，选 `rootfs-only`
5. 如果只是想先看看会做什么，先加 `--dry-run`
