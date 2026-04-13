# Conch Image Workflow Design

## 1. 目标与范围

本文档统一描述 Conch image 模块从构建侧到运行侧的整体设计。

本文档覆盖以下命令与流程：

- `conch build`
- `conch unpack`
- `conch pull`
- `conch pack`
- `conch push`

本文档同时覆盖以下镜像与产物类型：

- OCI rootfs
- EROFS rootfs
- kernel 镜像
- rootfs 镜像
- sandbox-image
- sandbox-snapshot

其中：

- `sandbox-image` 表示由 `rootfs + kernel` 组成的普通启动镜像
- `sandbox-snapshot` 表示由 `rootfs + kernel + mem-snapshot` 组成的快照启动镜像

## 2. 术语约定

### 2.1 rootfs 类型

本文档区分两类 rootfs：

1. OCI rootfs
- 指标准 OCI 镜像中的 rootfs 内容
- 主要作为构建输入存在

2. EROFS rootfs
- 指经过 Conch 转换后的 EROFS 层格式 rootfs
- 用于构建 Conch 可消费的 rootfs 镜像与后续运行时输入

### 2.2 kernel 镜像

kernel 镜像用于承载 kernel 与 initrd，是启动侧镜像的统一表达方式。

建议命名规则：

- x86:
  - `conch/kernel:6.6.0`
- arm:
  - `conch/kernel:6.6.0-aarch`

kernel 镜像包含 kernel 与 initrd，是 sandbox 启动链路中的标准输入。

### 2.3 sandbox

`sandbox` 表示启动侧镜像及相关组件，是镜像编排中的标准组件类型。

在镜像元数据中使用如下组件类型标识：

- `io.conch.kind=sandbox`

### 2.4 最终产物类型

最终产物统一为以下两类：

1. `sandbox-image`
- 组成：`rootfs + kernel`
- 用于普通启动场景

2. `sandbox-snapshot`
- 组成：`rootfs + kernel + mem-snapshot`
- 用于快照恢复启动场景

## 3. 总体链路

Conch image 模块覆盖从构建到运行前准备的完整链路：

1. 准备 OCI rootfs 或构建 OCI rootfs
2. 转换为 EROFS rootfs
3. 构建 rootfs 镜像
4. 构建 kernel 镜像
5. 组装 `sandbox-image` 或 `sandbox-snapshot`
6. 推送镜像
7. 拉取镜像
8. 解包到本地 containerd
9. 恢复本地 snapshot 关系，供运行侧使用

## 4. 命令与职责

### 4.1 `conch build`

`conch build` 负责从 Dockerfile 或构建上下文出发构建 Conch 镜像产物。

支持的扩展指令包括：

- `KERNEL <kernel_file> <initrd_file>`
- `INDEX`
- `SNAP`

对应的产物语义为：

1. `KERNEL`
- 构建 kernel 镜像

2. `KERNEL + INDEX`
- 生成 `sandbox-image`
- rootfs 先使用临时 tag 构建，再与 kernel 镜像组装为 `-t` 指定的最终镜像

3. `KERNEL + SNAP`
- 生成 `sandbox-snapshot`

### 4.2 `conch pack`

`conch pack` 面向标准 OCI 镜像输入，用于将标准 OCI rootfs 加工为 Conch 原生的 `sandbox-image`。

建议形式：

```bash
conch pack <oci-image> --kernel <bzImage> <initrd> -t <sandbox-image-name>
```

也可扩展：

```bash
conch pack <oci-image> --kernel-image <kernel-image> -t <sandbox-image-name>
```

### 4.3 `conch push`

`conch push` 提供统一的推送入口，用于发布 Conch 原生产物。

处理对象包括：

- `sandbox-image`
- `sandbox-snapshot`

建议形式：

```bash
conch push <local-conch-image> <registry>/<repo>:<tag>
```

`conch push` 底层使用 `buildah manifest push --all`，目标镜像会自动使用 `docker://` transport。认证建议通过 `buildah login <registry>` 预先完成。

### 4.4 `conch pull`

`conch pull` 提供统一的拉取入口。

`conch pull` 支持两类输入：

1. Conch 原生镜像
2. 标准 OCI 镜像

### 4.5 `conch unpack`

`conch unpack` 作为底层解包与恢复能力保留，用于：

- 本地已有镜像时的单独解包
- 调试与排障
- 作为 `conch pull` 的底层能力复用

## 5. 构建侧设计

### 5.1 OCI rootfs 到 EROFS rootfs

标准 OCI 镜像不能直接作为 Conch 最终运行态输入，需要经过 rootfs 格式转换。

统一处理步骤如下：

1. 拉取或构建 OCI rootfs
2. 提取 rootfs 内容
3. 转换为 EROFS rootfs
4. 执行 2MB 对齐
5. 基于 EROFS rootfs 构建 rootfs 镜像

其中：

- EROFS 转换与 2MB 对齐属于标准步骤
- 不视为某个命令路径下的可选优化

### 5.2 rootfs 镜像

rootfs 镜像由 EROFS rootfs 构建而来，是 sandbox 侧镜像组装时的标准输入。

### 5.3 kernel 镜像

kernel 镜像由 kernel 与 initrd 组成，是启动侧输入。

在 `conch build` 与 `conch pack` 中，kernel 镜像均作为标准输入使用。

默认 kernel 镜像采用 multi-arch tag 发布。构建时可通过 `internal/image/conchbuild/kernel/build-kernel-image.sh` 从不同架构的 kernel 目录生成统一镜像：

```bash
bash internal/image/conchbuild/kernel/build-kernel-image.sh \
  --x86-dir ./kernel-x86 \
  --arm-dir ./kernel-arm \
  --repo hub.oepkgs.net/conch/kernel \
  --version 6.6.0 \
  --push
```

其中 `--x86-dir` 与 `--arm-dir` 指向的目录均需包含：

- `bzImage`
- `conch.initrd`

脚本会生成并推送统一 tag：

```text
hub.oepkgs.net/conch/kernel:6.6.0
```

该 tag 可作为 `image.default_kernel_image` 的默认值供 `conch pull` 转换标准 OCI 镜像时使用。

### 5.4 当前构建过程中的主要产物

构建过程会涉及以下镜像与中间产物：

- OCI rootfs
- EROFS rootfs
- rootfs 镜像
- kernel 镜像
- 最终 index 镜像

### 5.5 对外统一产物命名

对外产物统一使用：

- `rootfs image`
- `kernel image`
- `sandbox-image`
- `sandbox-snapshot`

其中：

- `sandbox-image = rootfs + kernel`
- `sandbox-snapshot = rootfs + kernel + mem-snapshot`

## 6. 分发侧设计

### 6.1 `conch push`

`conch push` 用于推送 Conch 原生产物到远端仓库。

其职责是为以下镜像提供统一分发入口：

- `sandbox-image`
- `sandbox-snapshot`

### 6.2 `conch pull`

`conch pull` 用于拉取 Conch 原生产物，或将标准 OCI 镜像拉取并转换为 Conch 可运行输入。

#### A. 输入为 Conch 原生镜像

执行：

1. `ctr pull`
2. `conch unpack`

#### B. 输入为标准 OCI 镜像

执行：

1. 将标准 OCI 镜像拉取到本地镜像存储
2. 判断该镜像不属于 Conch 原生镜像格式
3. 读取默认配置中的 kernel 镜像，并结合命令行拉取参数
4. 将 OCI rootfs 转换为 EROFS rootfs，并执行 2MB 对齐
5. 构建 rootfs 镜像
6. 结合 kernel 镜像组装为 `sandbox-image`
7. 将生成的 `sandbox-image` 导入 containerd
8. 执行 unpack，在本地形成可运行状态

### 6.3 `conch pull` 的配置来源

`conch pull` 与 `conch unpack` 统一使用以下配置：

- `containerd.socket`
- `containerd.default_namespace`

对于标准 OCI 镜像转换路径，还使用以下 image 配置：

- `image.default_kernel_image`

其中：

- `image.default_kernel_image` 用于指定标准 OCI 镜像转换时默认使用的 kernel 镜像，默认值为 `hub.oepkgs.net/conch/kernel:6.6.0`

默认 kernel 镜像应发布为 multi-arch tag，例如同一个 `hub.oepkgs.net/conch/kernel:6.6.0` 下同时包含 `linux/amd64` 与 `linux/arm64`，由镜像拉取流程选择匹配当前机器架构的镜像。

当前实现中，如标准 OCI 镜像或默认 kernel 镜像拉取需要认证，`conch pull` 通过命令行参数传入认证信息；`plain-http` 也通过命令行参数显式指定，而不固化在配置模型中。

其中：

- 源镜像拉取参数使用 `--plain-http` 与 `--user`
- 默认 kernel 镜像拉取参数使用 `--kernel-plain-http` 与 `--kernel-user`

## 7. 镜像类型识别

### 7.1 Conch 原生镜像

Conch 原生镜像满足以下条件：

1. 镜像本身是 OCI index
2. index 成员 descriptor 上包含 Conch 组件 annotation
3. 能识别出合法组件组合

组件类型包括：

- `io.conch.kind=rootfs`
- `io.conch.kind=sandbox`
- `io.conch.kind=mem-snapshot`

普通启动镜像至少包含：

- `rootfs`
- `sandbox`

快照启动镜像包含：

- `rootfs`
- `sandbox`
- `mem-snapshot`

### 7.2 标准 OCI 镜像

标准 OCI 镜像只具备 OCI rootfs 语义，不包含：

- sandbox 组件
- mem-snapshot
- Conch 原生镜像结构

因此不能直接进入 `conch unpack`，需要先转化为 Conch 原生镜像。

### 7.3 判断顺序

`conch pull` 在处理输入镜像时按如下顺序判断：

1. 检查输入对象是否为 OCI index
2. 检查成员 descriptor 上是否存在合法的 Conch 组件 annotation
3. 若可识别合法组件组合，则按 Conch 原生镜像处理
4. 否则按标准 OCI 镜像处理
5. 若无法满足任一路径要求，则直接报错

## 8. 运行侧设计

### 8.1 `conch unpack`

`conch unpack` 负责将 Conch 原生镜像解包到 containerd，并恢复 rootfs 与相关组件之间的本地关联关系。

### 8.2 namespace 统一

Conch config 文件中包含 namespace 配置，该配置对应 containerd namespace，并影响 conchd 工作目录与本地资源组织方式。

image 相关命令统一遵循以下优先级：

1. CLI 显式指定的 namespace
2. config 文件中的 namespace
3. 若 config 为 `""`
   - 继续下传空字符串
   - 由底层模块兜底为 `default`

这一规则覆盖：

- `conch unpack`
- `conch pull`
- `conch pack`
- `conch build` 中涉及 containerd / snapshot 的流程

## 9. `conch pack` 详细设计

### 9.1 默认命名规则

当未指定 `-t` 时，可基于输入镜像生成默认名称，例如：

```text
sandbox-image-<source-name>:<source-tag>
```

不额外引入新的 `conch-` 前缀。

### 9.2 内部流程

`conch pack` 的内部流程包括：

1. 拉取标准 OCI 镜像
2. 读取 OCI rootfs
3. 转换为 EROFS rootfs，并执行 2MB 对齐
4. 构建 rootfs 镜像
5. 构建或复用 kernel 镜像
6. 组装 `sandbox-image`

### 9.3 `conch-agent` 增量替换场景

对于已有老镜像，但只希望替换 `conch-agent` 或其他少量文件、重新验证运行的场景，也纳入 `conch pack` 能力统一考虑。

典型情况包括：

- 老镜像中的 `conch-agent` 版本较旧
- 新版 `conch-agent` 已重新编入 initrd
- 希望尽量复用原有 rootfs，仅替换 kernel/initrd 或少量运行时内容后重新得到新的 `sandbox-image`

这类场景本质上属于：

- 复用已有 OCI rootfs
- 更新 kernel/initrd 或相关运行时内容
- 重新组装为新的 `sandbox-image`

## 10. Dockerfile 扩展设计

`conch build` 的 Dockerfile 扩展采用以下语义：

1. `KERNEL`
- 构建 kernel 镜像

2. `KERNEL + INDEX`
- 生成 `sandbox-image`
- rootfs 会先转换为 PMEM/EROFS rootfs 镜像，再与 kernel 镜像组装为 Conch index
- 适用于只需要生成可分发 Conch 镜像、不需要立即创建热启动快照的场景

3. `KERNEL + SNAP`
- 生成 `sandbox-snapshot`

这样可以使构建命令与最终产物类型保持直接对应。

## 11. 总结

Conch image 模块覆盖从构建侧到运行侧的完整链路：

- 从 OCI rootfs / EROFS rootfs 构建产物
- 生成 kernel 镜像
- 组装 `sandbox-image` / `sandbox-snapshot`
- 统一 push / pull / unpack 入口
- 在目标机侧恢复为可运行状态

文档、命令与产物统一采用以下命名：

- `rootfs`
- `kernel`
- `sandbox`
- `sandbox-image`
- `sandbox-snapshot`

该命名体系直接对应实际组成关系，可覆盖完整的 image workflow。
