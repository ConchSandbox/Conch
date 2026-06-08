# Conch Image Workflow Design

## 1. 目标与范围

本文档描述当前 Conch image 模块的 native EROFS 镜像流程，覆盖从已有 OCI rootfs 镜像转换、组件组装、推送，到目标机拉取与解包的完整链路。

当前核心命令包括：

- `conch convert`
- `conch push`
- `conch pull`
- `conch unpack`
- `conch snapshot export`

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

### 3.1 `conch convert`

`conch convert` 从已有 OCI rootfs 镜像生成 Conch 原生 boot index：

```bash
conch convert --source docker.io/library/nginx:latest \
  --kernel ./bzImage \
  --initrd ./conch.initrd \
  -t localhost/conch/nginx:latest
```

内部流程如下：

1. CLI 将 `source`、`kernel`、`initrd`、`tag` 等参数提交给 conchd 的 `/api/image/convert`。
2. conchd 在进程内 containerd 中查找 `source`；本地不存在时从 registry 拉取。
3. rootfs 通过 `erofs-container-toolkit` 转换为 native EROFS layer。
4. conchd 将转换后的 rootfs manifest、kernel/initrd 生成的 sandbox manifest 组装为同一个 boot index。
5. conchd 直接将新增 blob 写入 containerd content store，创建用户指定 tag 的 image record，然后 unpack 完整 boot index。

### 3.2 rootfs 到 native EROFS layer

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

### 3.3 kernel/initrd 组件镜像

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

### 3.4 boot index 组装方式

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

### 4.1 `conch push`

`conch push` 将本地 conchd/containerd 中的 Conch boot index 推送到 registry：

```bash
conch push localhost/conch/nginx:latest registry.example.com/conch/nginx:latest
```

如目标 registry 需要明文 HTTP 或认证，可使用：

```bash
conch push --plain-http --username <user> --password <password> \
  localhost/conch/nginx:latest registry.example.com/conch/nginx:latest
```

### 4.2 `conch pull`

目标机使用 `conch pull` 拉取 Conch 原生镜像：

```bash
conch pull registry.example.com/conch/nginx:latest
```

`conch pull` 会自动：

1. 拉取 boot index。
2. 识别 `rootfs`、`sandbox`、`mem-snapshot` 组件。
3. 将各组件 unpack 到 `erofs` snapshotter。
4. 写入 rootfs snapshot labels，恢复 rootfs 与 sandbox/mem snapshot 的关联。

### 4.3 `conch unpack`

当 Conch 原生镜像已经存在于本地 containerd 时，可以单独执行：

```bash
conch unpack localhost/conch/nginx:latest
```

该命令主要用于调试或重复恢复本地 snapshot 关系。

## 5. 快照镜像

`conch convert --snapshot` 会在转换 rootfs 和 sandbox 组件后，创建一个 sandbox，执行 pause，并将生成的 mem snapshot 一起导出为 `sandbox-snapshot`：

```bash
conch convert --source docker.io/library/nginx:latest \
  --kernel ./bzImage \
  --initrd ./conch.initrd \
  --snapshot \
  -t localhost/conch/nginx-snapshot:latest
```

本地已有运行态 snapshot 或 sandbox 时，也可以使用：

```bash
conch snapshot export --snapshot-id <rootfs-snapshot-id> -t localhost/conch/snapshot:latest
conch snapshot export --sandbox-id <sandbox-id> -t localhost/conch/snapshot:latest
```

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
conch pull registry.example.com/conch/nginx:latest
```

如果需要验证完整转换链路，则在构建机执行 `conch convert` 和 `conch push`，在目标机执行 `conch pull`。
