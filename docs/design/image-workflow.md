# Conch Image Workflow Design

## 1. 目标与范围

本文档描述当前 Conch image 模块的 native EROFS 镜像流程，覆盖从已有 OCI rootfs 镜像转换、组件组装、推送，到目标机拉取与解包的完整链路。

当前核心命令包括：

- `conch template create`
- `conch sandbox checkpoint`
- `conch image push`
- `conch image pull`
- `conch image unpack`

conchd 会在进程内初始化 containerd v2 服务和 Conch 插件，不要求单独启动系统 `containerd` 守护进程。

上述 image workflow 命令均支持通过 `-n/--namespace` 指定 containerd namespace；未指定时使用配置中的默认 namespace，并最终兜底为 `default`。

## 2. 镜像与组件

Conch 原生镜像使用 OCI image/index 作为外层结构，内部组件通过 descriptor annotation 区分：

- `io.conch.kind=rootfs`
- `io.conch.kind=sandbox`
- `io.conch.kind=mem-snapshot`

最终产物分为两类：

1. `sandbox-image`
- 组成：`rootfs + sandbox`
- 用于普通启动

2. `sandbox-snapshot`
- 组成：`rootfs + sandbox + mem-snapshot`
- 用于快照恢复启动

其中 `sandbox` 组件承载 kernel 与 initrd。

## 3. 构建/转换流程

### 3.1 `conch template create`

`conch template create` 从已有 OCI rootfs 镜像生成 Conch 原生 boot index，并登记为可启动 Template：

```bash
conch template create --source docker.io/library/nginx:latest \
  --kernel ./bzImage \
  --initrd ./conch.initrd \
  -t localhost/conch/nginx:latest
```

内部流程如下：

1. CLI 将 `source`、`kernel`、`initrd`、`tag` 等参数提交给 conchd 的 `/api/template/create`。
2. conchd 在进程内 containerd 中查找 `source`；本地不存在时从 registry 拉取。
3. rootfs 通过 `erofs-container-toolkit` 转换为 native EROFS layer。
4. conchd 将转换后的 rootfs manifest、kernel/initrd 生成的 sandbox manifest 组装为同一个 boot index。
5. conchd 直接将新增 blob 写入 containerd content store，创建用户指定 tag 的 image record，并按 digest 静态校验完整 boot index。
6. 构建、发布和校验全部成功后，conchd 一次性登记包含不可变 boot index digest 的 `origin=image` Template；此流程不预先创建 Sandbox snapshot。
7. 创建 Sandbox 时，Sandbox 模块读取 Template，按 digest 解析并 unpack boot index，再直接调用 Snapshot 模块创建 cold boot layout 或恢复 resume boot layout。

### 3.2 `conch sandbox checkpoint`

`conch sandbox checkpoint <sandbox-id>` 捕获一个运行中或已暂停 Sandbox 的状态，并生成新的可恢复 Template。Checkpoint 是 Sandbox 动作，不是独立资源：

1. Sandbox 短暂暂停并捕获内存状态，随后发布包含 rootfs/mem/vm refs 的 boot index。
2. conchd 校验已发布 boot index 的 digest、VMM 和内存规格。
3. 只有捕获、发布和校验全部成功后，conchd 才原子写入完整的 `origin=checkpoint`、`boot_mode=resume` Template，并推进 Sandbox 的 checkpoint head；失败时不留下半成品 Template 记录。
4. 产物统一通过 `conch template ls/inspect/rm` 管理。

### 3.3 rootfs 到 native EROFS layer

rootfs 转换由 `erofs-container-toolkit` 接入 containerd image converter 完成：

1. 读取标准 OCI rootfs image 的 manifest/layers。
2. 对每个 rootfs layer 执行 EROFS 转换。
3. `mkfs.erofs` 使用默认参数：

```text
--fsalignblks=512
```

4. 转换后的 layer media type 统一为：

```text
application/vnd.erofs.layer.v1
```

5. 转换完成后校验 layer 非空，并按 2MiB 对齐。
6. 转换后的 rootfs image 会被 unpack 到 `erofs` snapshotter，得到 rootfs snapshot key。

因此运行环境需要 `erofs-utils` 支持 `mkfs.erofs --fsalignblks`，建议使用 1.9 或更新版本。

### 3.4 kernel/initrd 组件镜像

conchd 会将 kernel/initrd 临时放入一个目录结构：

```text
boot/vmlinuz
data/conch.initrd
```

然后调用 `mkfs.erofs` 将该目录转换成 native EROFS layer，并写入 containerd content store。该 manifest 的 descriptor 会带上：

```text
io.conch.kind=sandbox
```

该组件在启动时提供：

- kernel path
- initrd path
- sandbox/VM 相关 snapshot view

### 3.5 boot index 组装方式

最终 boot index 是一个 OCI image index。普通启动镜像包含两个 manifest descriptor：

1. rootfs manifest
- annotation：`io.conch.kind=rootfs`
- ref name：`<tag>-rootfs`

2. sandbox manifest
- annotation：`io.conch.kind=sandbox`
- ref name：`<tag>-sandbox`

快照启动镜像会额外包含：

3. mem-snapshot manifest
- annotation：`io.conch.kind=mem-snapshot`
- ref name：`<tag>-mem`

## 4. 分发与目标机准备

### 4.1 `conch image push`

`conch image push` 将本地 conchd/containerd 中的 Conch boot index 推送到 registry：

```bash
conch image push localhost/conch/nginx:latest registry.example.com/conch/nginx:latest
```

如目标 registry 需要明文 HTTP 或认证，可使用：

```bash
conch image push --plain-http --username <user> --password <password> \
  localhost/conch/nginx:latest registry.example.com/conch/nginx:latest
```

### 4.2 `conch image pull`

目标机使用 `conch image pull` 拉取 Conch 原生镜像：

```bash
conch image pull registry.example.com/conch/nginx:latest
```

`conch image pull` 会自动：

1. 拉取 boot index。
2. 识别 `rootfs`、`sandbox`、`mem-snapshot` 组件。
3. 将各组件 unpack 到 `erofs` snapshotter。
4. 写入 rootfs snapshot labels，恢复 rootfs 与 sandbox/mem snapshot 的关联。

传入 `--skip-unpack` 时只执行拉取和完整 content fetch，不生成 snapshot，也不写入 snapshot labels；之后可显式运行 `conch image unpack`。

### 4.3 `conch image unpack`

当 Conch 原生镜像已经存在于本地 containerd 时，可以单独执行：

```bash
conch image unpack localhost/conch/nginx:latest
```

该命令主要用于调试或重复恢复本地 snapshot 组件。

## 5. Template checkpoint

Template 创建已经覆盖 boot image 转换和 Template 注册。

运行态状态捕获通过 `conch sandbox checkpoint` 完成，结果注册为可恢复 Template。底层 snapshot 不再提供按 rootfs key 导出的接口。

## 6. 目标机验证

目标机需要满足：

- `mkfs.erofs` 支持 `--fsalignblks`
- kernel 支持 EROFS
- 具备 root/mount 权限
- 安装并可执行 `cloud-hypervisor`

建议验证：

```bash
mkfs.erofs -V
mkfs.erofs --help | grep fsalignblks
grep -w erofs /proc/filesystems
which cloud-hypervisor
```

然后启动 conchd，拉取并解包镜像：

```bash
./bin/conchd -config ./config/config.yaml
conch image pull registry.example.com/conch/nginx:latest
```

如果需要验证完整转换链路，则在构建机执行 `conch template create` 和 `conch image push`，在目标机执行 `conch image pull`。
