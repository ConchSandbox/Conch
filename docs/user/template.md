# Conch Template 与镜像指南

Template 是用于创建 Sandbox 的可启动 OCI 制品，使用 `conch template` 管理。

- Template Name 与 Platform 一起标识本地模板，同名模板可以分别保存 x86_64 和 ARM64 版本。
- Template ID 是内容 digest，用于选择一个确定版本。

大多数 Template 命令使用 Name；创建 Sandbox 时也可以直接指定 ID。

## 1. 创建 Template

创建时必须指定 Name：

```bash
conch template create \
  --name localhost/conch/openeuler:latest \
  --source docker.io/openeuler/openeuler:24.03-lts-sp2 \
  --kernel /var/lib/conch/kernel \
  --initrd /var/lib/conch/conch.initrd
```

输出包含 Name 和其当前 ID：

```console
Template Name: localhost/conch/openeuler:latest
Template ID: sha256:1111...
```

再次使用相同 `--name` 和平台创建时，会更新对应平台的 Template。

`create` 支持 `--platform linux/amd64`（x86_64）和 `--platform linux/arm64`（AArch64）。
未指定时使用 **conchd 所在机器**的平台。也接受 `amd64`、`x86_64`、`arm64`、
`aarch64` 等架构别名，统一保存为 `linux/amd64` 或 `linux/arm64`。

例如，在 x86_64 机器上打包 ARM64 模板：

```bash
conch template create \
  --platform linux/arm64 \
  --name localhost/conch/openeuler:latest \
  --source docker.io/openeuler/openeuler:24.03-lts-sp2 \
  --kernel ./arm64/Image \
  --initrd ./arm64/conch.initrd
```

构建会选择指定平台的 rootfs，转换为 EROFS，并为全部组件记录目标平台。
本地 source 缺少该平台时，会从仓库下载。kernel 和 initrd 必须由调用者提供目标架构
对应的文件；打包不会交叉编译这些文件，也不会执行目标架构的程序。

所有 CLI 请求共用 `CONCH_API_TIMEOUT`，默认 2 分钟。首次转换较大镜像时可用：

```bash
CONCH_API_TIMEOUT=30m conch template create \
  --name localhost/conch/openeuler:latest \
  --source hub.oepkgs.net/openeuler/python:latest \
  --kernel /var/lib/conch/kernel \
  --initrd /var/lib/conch/conch.initrd
```

## 2. 查看和删除

```console
$ conch template ls
NAME                              PLATFORM     TEMPLATE_ID    ORIGIN  BOOT_MODE  SOURCE_REF  SOURCE_SANDBOX
localhost/conch/openeuler:latest    linux/amd64  sha256:1111... image   cold       ...         -
localhost/conch/openeuler:latest    linux/arm64  sha256:2222... image   cold       ...         -

$ conch template inspect localhost/conch/openeuler:latest

$ conch template rm localhost/conch/openeuler:latest
Removed template: localhost/conch/openeuler:latest
```

`rm` 删除指定的本地 Template。

`ls` 默认列出所有平台，也可以使用 `conch template ls --platform linux/arm64` 过滤。
`inspect`、`rm`、`push` 和 `unpack` 未指定平台时，只选择 conchd 所在机器的平台；
不会自动回退到同名的其他架构。显式选择 ARM64 模板：

```bash
conch template inspect --platform linux/arm64 localhost/conch/openeuler:latest
conch template rm --platform linux/arm64 localhost/conch/openeuler:latest
```

删除 ARM64 版本不会影响同名的 x86_64 版本。已有的旧格式本地模板会从 OCI 配置中
识别架构，可以继续查看、更新和删除。

## 3. 分发

`template pull` 直接使用 registry reference 作为本地 Template Name。远端 tag
更新后再次 pull，同一个 Name 与 Platform 会指向新的 Template ID：

```console
$ conch template pull registry.example.com/conch/openeuler:latest
Template Name: registry.example.com/conch/openeuler:latest
Template ID: sha256:2222...

$ conch template push registry.example.com/conch/openeuler:latest mirror.example.com/conch/openeuler:stable
Pushed template: registry.example.com/conch/openeuler:latest -> mirror.example.com/conch/openeuler:stable
```

`template push`、`inspect`、`unpack` 和 `rm` 都以 Template Name 选择当前目标。

使用 `--platform` 跨架构下载，同名的两个版本可以共存：

```bash
conch template pull --platform linux/amd64 registry.example.com/conch/openeuler:latest
conch template pull --platform linux/arm64 registry.example.com/conch/openeuler:latest
conch template push --platform linux/arm64 \
  registry.example.com/conch/openeuler:latest mirror.example.com/conch/openeuler:arm64
```

远端可以是单个平台的 Boot Index，也可以是 OCI 多架构索引（每个带 `platform` 的
条目指向一个 Boot Index）。下载会选择匹配的平台，并下载该模板的全部组件；平台
不存在或组件架构不一致时会报错。普通容器镜像仍通过 `conch image pull` 管理。

`push --platform` 推送所选平台的一个 Boot Index，不会自动合并远端 tag 下的其他
平台。分别发布时应使用不同远端 tag；需要共用一个远端 tag 时，由仓库侧 OCI 多架构
索引引用这些 Boot Index。此处的本地同名共存不要求远端使用多架构索引。

## 4. Sandbox 与 checkpoint

Sandbox 可以通过 Name 或 ID 创建，但二者只能指定一个。Name 使用其当前版本，ID
固定选择指定版本：

```bash
conch sandbox create --template-name registry.example.com/conch/openeuler:latest

# 或直接使用不可变 ID
conch sandbox create --template-id sha256:1111...
```

按 Name 创建 Sandbox 时自动选择 conchd 所在机器的平台。跨架构模板可以下载、打包和
推送；`unpack` 和 Sandbox 启动要求模板平台与宿主平台匹配，不提供跨架构执行能力。

Name 后续更新只影响新建 Sandbox。checkpoint 必须指定保存结果的 Name：

```bash
conch sandbox checkpoint \
  --template-name localhost/conch/checkpoint-sandbox-123:latest \
  sandbox-123
```
