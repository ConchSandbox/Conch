# Conch Template 与镜像指南

Template 是用于创建 Sandbox 的可启动 OCI 制品，使用 `conch template` 管理。

- Template Name 用于日常管理，可以更新到新版本。
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

再次使用相同 `--name` 创建时，会更新现有 Template。

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
NAME                                      TEMPLATE_ID    ORIGIN  BOOT_MODE  SOURCE_REF  SOURCE_SANDBOX
localhost/conch/openeuler:latest          sha256:1111... image   cold       ...         -

$ conch template inspect localhost/conch/openeuler:latest

$ conch template rm localhost/conch/openeuler:latest
Removed template: localhost/conch/openeuler:latest
```

`rm` 删除指定的本地 Template。

## 3. 分发

`template pull` 直接使用 registry reference 作为本地 Template Name。远端 tag
更新后再次 pull，同一个 Name 会指向新的 Template ID：

```console
$ conch template pull registry.example.com/conch/openeuler:latest
Template Name: registry.example.com/conch/openeuler:latest
Template ID: sha256:2222...

$ conch template push registry.example.com/conch/openeuler:latest mirror.example.com/conch/openeuler:stable
Pushed template: registry.example.com/conch/openeuler:latest -> mirror.example.com/conch/openeuler:stable
```

`template push`、`inspect`、`unpack` 和 `rm` 都以 Template Name 选择当前目标。

## 4. Sandbox 与 checkpoint

Sandbox 可以通过 Name 或 ID 创建，但二者只能指定一个。Name 使用其当前版本，ID
固定选择指定版本：

```bash
conch sandbox create --template-name registry.example.com/conch/openeuler:latest

# 或直接使用不可变 ID
conch sandbox create --template-id sha256:1111...
```

Name 后续更新只影响新建 Sandbox。checkpoint 必须指定保存结果的 Name：

```bash
conch sandbox checkpoint \
  --template-name localhost/conch/checkpoint-sandbox-123:latest \
  sandbox-123
```
