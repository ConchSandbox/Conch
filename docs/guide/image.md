# Conch Image Guide

本文档介绍当前 Conch 镜像管理的常用命令。当前镜像入口围绕 `conch convert`、`conch pull`、`conch push`、`conch unpack`、`conch image` 和 `conch snapshot` 展开。

## 1. conch convert

`conch convert` 用于把已有 OCI rootfs 镜像与本地 kernel/initrd 文件转换为 Conch boot OCI index。

```bash
conch convert \
  --source docker.io/openeuler/openeuler:24.03-lts-sp2 \
  --kernel ./bzImage \
  --initrd ./conch.initrd \
  -t localhost/conch/openeuler:latest
```

如果需要生成包含内存快照的 `sandbox-snapshot` 镜像，可增加 `--snapshot`：

```bash
conch convert \
  --source docker.io/openeuler/openeuler:24.03-lts-sp2 \
  --kernel ./bzImage \
  --initrd ./conch.initrd \
  --snapshot \
  -t localhost/conch/openeuler-snapshot:latest
```

## 2. conch snapshot export

`conch snapshot export` 用于将已有 rootfs snapshot 或运行中的 sandbox 导出为 `sandbox-snapshot` 镜像。

```bash
conch snapshot export \
  --snapshot-id sha256:xxxxxxxx \
  -t localhost/conch/sandbox-snapshot:latest
```

也可以通过 sandbox ID 触发 pause 后导出：

```bash
conch snapshot export \
  --sandbox-id sandbox-123 \
  -t localhost/conch/sandbox-snapshot:latest
```

`--snapshot-id` 与 `--sandbox-id` 二选一，`-t` / `--tag` 必填。

## 3. conch pull / push / unpack

`conch pull` 用于拉取 Conch 镜像，并在本地完成 unpack：

```bash
conch pull hub.oepkgs.net/conch/openeuler:cri-v0.0.1-x86
```

`conch push` 用于推送本地 Conch OCI index：

```bash
conch push localhost/conch/openeuler:latest hub.oepkgs.net/conch/openeuler:latest
```

`conch unpack` 用于把已有 Conch boot OCI index 解包到 conchd 管理的 containerd store：

```bash
conch unpack hub.oepkgs.net/conch/openeuler:cri-v0.0.1-x86
```

这些命令会通过 conchd API 操作 conchd 进程内的 containerd store，支持通过配置读取 `server.unix_socket` 或 `server.host` / `server.port`，以及 `containerd.default_namespace`。

## 4. conch image

`conch image ls` 展示 conchd/containerd 中的 image metadata：

```bash
conch image ls
```

输出包含：

```text
NAME  KIND  DIGEST  SIZE
```

其中 `KIND` 可用于区分 `sandbox-base`、`sandbox-snapshot`、`rootfs`、`sandbox`、`mem-snapshot` 等镜像类型。

`conch image rm` 删除 containerd image record：

```bash
conch image rm localhost/conch/openeuler:latest
```

注意：`image rm` 不会自动清理该 image unpack 产生的 snapshot 数据。如需清理 snapshot，请使用 `conch snapshot rm`。

## 5. conch snapshot

`conch snapshot ls` 展示 erofs snapshotter 中的 snapshot：

```bash
conch snapshot ls
```

输出中的 `CONCH_ROLE` 会标识 Conch 关系角色：`rootfs` 表示 rootfs/mem/vm 关联组的 rootfs anchor，`mem` 与 `vm` 表示带有明确 component kind label 的组件，`unknown` 表示带有 `group-id` 但没有可识别 component kind 的组件，`standalone` 表示未加入 Conch 关联组的普通 snapshot。`GROUP_ID` 是该 snapshot 所属 group 的 rootfs snapshot key，`standalone` snapshot 的 `GROUP_ID` 为空。

`conch snapshot rm` 默认只删除未被 Conch rootfs/mem/vm 关系引用的单个 snapshot：

```bash
conch snapshot rm sha256:xxxxxxxx
```

对于 Conch snapshot group，使用 `CONCH_ROLE=rootfs` 的 rootfs anchor key 作为入口，通过 `--cascade` 删除完整的 rootfs/mem/vm 关联组：

```bash
conch snapshot rm --cascade sha256:rootfs-snapshot
```

如果直接删除已关联的 mem/vm 组件，命令会拒绝操作，避免留下悬空 group ref。

`conch snapshots` 是 `conch snapshot` 的兼容 alias。

## 6. 相关文档

- 镜像工作流设计：`docs/design/image-workflow.md`
- CRI 使用文档：`docs/guide/cri.md`
