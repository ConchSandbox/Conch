# Conch Image Guide

本文档介绍当前 Conch 镜像管理的常用命令。当前镜像入口围绕 `conch template`、`conch image pull`、`conch image push`、`conch image unpack`、`conch image` 和 `conch debug snapshot` 展开。

## 1. conch template create

`conch template create` 用于把已有 OCI rootfs 镜像与本地 kernel/initrd 文件转换为可启动 Template，并返回 `tmpl_xxx`。使用conch的rpm包安装conch，rpm包中包含kernel及conch.initrd文件，对应的指令如下。

```
conch template create \
  --source docker.io/openeuler/openeuler:24.03-lts-sp2 \
  --kernel /var/lib/conch/kernel \
  --initrd /var/lib/conch/conch.initrd \
  -t localhost/conch/openeuler:latest
```
所有 Template（包括 `conch sandbox checkpoint` 产生的可恢复 Template）统一通过以下命令管理：

```bash
conch template ls
conch template ls --origin checkpoint --boot-mode resume
conch template inspect <tmpl_id>
conch template rm <tmpl_id>
```

## 2. conch image pull / push / unpack

以下命令中的 `registry.example.com` 是占位域名，使用时需替换为实际镜像仓库。

`conch image pull` 用于拉取 Conch 镜像，并在本地完成 unpack：

```bash
conch image pull registry.example.com/conch/openeuler:latest
```

如果只需要下载 OCI content、暂时不生成本地 snapshot：

```bash
conch image pull --skip-unpack registry.example.com/conch/openeuler:latest
```

`conch image push` 用于推送本地 Conch OCI index：

```bash
conch image push localhost/conch/openeuler:latest registry.example.com/conch/openeuler:latest
```

`conch image unpack` 用于把已有 Conch boot OCI index 解包到 conchd 管理的 containerd store：

```bash
conch image unpack registry.example.com/conch/openeuler:latest
```

这些命令会通过 conchd API 操作 conchd 进程内的 containerd store，支持通过配置读取 `server.unix_socket` 或 `server.host` / `server.port`，以及 `containerd.default_namespace`。使用 `--skip-unpack` 拉取后，可以稍后通过 `conch image unpack` 单独生成 snapshot。

## 3. conch image

`conch image ls` 展示 conchd/containerd 中的 image metadata：

```bash
conch image ls
```

输出包含：

```text
NAME  KIND  DIGEST  SIZE
```

其中 `KIND` 可用于区分普通 OCI 镜像与 `sandbox-base`、`sandbox-snapshot` 等顶层 Conch 镜像。默认输出隐藏构建过程中使用的 `rootfs`、`sandbox`、`mem-snapshot` component image records；排障时可使用 `conch image ls --all` 查看 conchd/containerd 中的全部 image records。

`conch image rm` 删除 containerd image record：

```bash
conch image rm localhost/conch/openeuler:latest
```

注意：`image rm` 不会自动清理该 image unpack 产生的底层 snapshot 数据。如需调试清理，请使用 `conch debug snapshot rm`。

## 4. conch debug snapshot

`conch debug snapshot ls` 展示 erofs snapshotter 中的底层 snapshot：

```bash
conch debug snapshot ls
```

输出只包含 containerd snapshotter 的底层字段：`KIND`、`KEY` 和 `PARENT`。Template store 只记录不可变的 boot index digest；创建 Sandbox 时才按 digest 解析 rootfs/mem/vm 组件，并由 Sandbox 模块直接请求 Snapshot 模块准备 boot layout。

`conch debug snapshot rm` 只删除命令中指定的单个 snapshot key，不推断或级联删除其他组件：

```bash
conch debug snapshot rm sha256:xxxxxxxx
```

这是面向开发和排障的原始操作，不检查 Template 或运行中 Sandbox 是否仍引用该 key。高层对象应通过 `conch template` 和 `conch sandbox` 管理。

旧 `conch snapshot` / `conch snapshots` 顶层入口已移除。底层 snapshot 排障入口只保留在 `conch debug snapshot` 下；普通用户应使用 Template 和 Sandbox 命令。

## 5. 相关文档

- 镜像工作流设计：`docs/design/image-workflow.md`
