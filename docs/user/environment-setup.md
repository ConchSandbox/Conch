# Conch 环境准备

适用于 Linux 5.10+（`x86_64`/`aarch64`）。主机需要可用的 `/dev/kvm`，并且至少有一条可用的 IPv4 默认路由；安装系统依赖时需要 root 权限。

## 必需工具

| 工具 | 要求 | 安装位置或来源 |
| --- | --- | --- |
| Go | 1.26.2+ | [官方下载](https://go.dev/doc/install)，命令加入 `PATH` |
| Git、GNU Make | 发行版版本 | 系统包管理器 |
| Cloud Hypervisor | v52.0-conch | [Conch release](https://github.com/ConchSandbox/cloud-hypervisor/releases/tag/v52.0-conch)，安装为 `/usr/local/bin/cloud-hypervisor` |
| erofs-utils | 1.9+，`mkfs.erofs` 支持 `--fsalignblks` | 系统包管理器或[源码安装](https://erofs.docs.kernel.org/en/latest/install.html)，命令加入 `PATH` |
| CNI plugins | `bridge`、`host-local` | [官方 release](https://github.com/containernetworking/plugins/releases)，安装到 `/usr/libexec/cni` |
| iptables、util-linux | 提供 `iptables`、`nsenter` | 系统包管理器 |
| kmod | 内核模块化构建时提供 `modprobe` | 系统包管理器 |

可选工具：

| 用途 | 工具 | 要求或位置 |
| --- | --- | --- |
| Python SDK | Python 3.10+、pip、venv | 系统包管理器；虚拟环境使用仓库内 `.venv` |
| `volume_mounts` | virtiofsd 1.13.3+ | [官方 release](https://gitlab.com/virtio-fs/virtiofsd/-/releases)，默认 `/usr/libexec/virtiofsd` |
| 修改 protobuf 接口 | protoc | 系统包管理器；其余插件由 `make gen-proto-*` 安装 |
| 构建 initramfs | cpio、gzip | 系统包管理器 |

## 安装

### 发行版软件仓库

openEuler、Fedora、CentOS、RHEL：

```bash
sudo dnf install -y git make curl tar \
  iptables util-linux kmod erofs-utils containernetworking-plugins
```

Debian、Ubuntu：

```bash
sudo apt-get update
sudo apt-get install -y git make curl tar \
  iptables util-linux kmod erofs-utils containernetworking-plugins
```

### Go

按照 [Go 官方安装说明](https://go.dev/doc/install) 安装 Go 1.26.2+，并将 `go` 加入 `PATH`。

### Cloud Hypervisor

从 [ConchSandbox release](https://github.com/ConchSandbox/cloud-hypervisor/releases/tag/v52.0-conch) 下载 Cloud Hypervisor v52.0-conch，安装为 `/usr/local/bin/cloud-hypervisor`：

| 主机架构 | Asset |
| --- | --- |
| `x86_64` | `cloud-hypervisor-static` |
| `aarch64` | `cloud-hypervisor-static-aarch64` |

### CNI plugins

Conch 默认从主机共享目录 `/usr/libexec/cni` 加载 `bridge`、`host-local`。发行版安装的插件如果位于其他目录，将实际路径写入 `network.cni.plugin_bin_dirs`；也可以从 [CNI plugins release](https://github.com/containernetworking/plugins/releases) 下载对应架构的包并解压到该目录。Conch 直接通过 netlink 启用 network namespace 中的 loopback 接口，不需要安装 `loopback` CNI 插件。

### erofs-utils

如果发行版提供的 `mkfs.erofs` 不支持 `--fsalignblks`，按照 [EROFS 官方说明](https://erofs.docs.kernel.org/en/latest/install.html) 从源码安装 1.9+，并将命令加入 `PATH`。

## 主机配置

安装仓库提供的 CNI 配置并加载所需内核模块：

```bash
sudo install -d -m 0755 /etc/conch/cni/net.d
sudo install -m 0644 config/cni/net.d/10-conch.conf /etc/conch/cni/net.d/10-conch.conf
sudo modprobe erofs
sudo modprobe vhost_vsock
```

Conch 将 libcni result cache 保存在 `<server.state_dir>/cni/results`；host-local IPAM 状态保存在 `<server.state_dir>/cni/networks`。这两个目录由 `server.state_dir` 派生，不单独暴露为用户配置接口。

使用前确认 CNI 配置的子网不与主机、集群或 guest tap 子网重叠。

启动 `conchd` 前，请确保主机存在可用的 IPv4 默认路由，例如 `default via 192.168.1.1 dev eth0`。Conch 启动网络池时会查找 `0.0.0.0/0` 默认路由并配置 CNI bridge 的转发规则；如果找不到默认路由，网络池初始化会失败，`conchd` 也不会启动成功。

如果二进制安装在其他位置，在本地配置中填写其绝对路径：

```yaml
sandbox:
  cloud_hypervisor:
    binary: /actual/path/to/cloud-hypervisor
volume:
  virtiofs:
    binary: /actual/path/to/virtiofsd
```

## 验证

```bash
go version
cloud-hypervisor --version
mkfs.erofs --help 2>&1 | grep -- --fsalignblks
test -x /usr/libexec/cni/bridge
test -x /usr/libexec/cni/host-local
test -r /dev/kvm -a -w /dev/kvm
grep -w erofs /proc/filesystems
test -e /dev/vhost-vsock
```

验证通过后，按照[快速开始](getting-started.md)构建并启动 Conch。
