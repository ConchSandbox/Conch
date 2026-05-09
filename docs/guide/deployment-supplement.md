# DEPLOYMENT_SUPPLEMENT

本文档补充 Conch 在部署阶段的常见系统配置建议。

## 推荐系统配置

以下配置建议在宿主机侧完成，用于保证宿主机与沙箱网络通信正常。

### 1. 开启宿主机到沙箱 IP 的转发能力

```bash
sysctl -w net.ipv4.ip_forward=1
```

说明：

- 未开启 `net.ipv4.ip_forward` 时，宿主机可能无法正常向沙箱 IP 转发流量。
- 如需持久生效，建议写入 `/etc/sysctl.d/*.conf` 或 `/etc/sysctl.conf` 后执行 `sysctl --system`。

### 2. 配置宿主机代理绕过沙箱网段

```bash
export no_proxy="$no_proxy,10.12.0.0/20"
```

说明：

- Conch 默认使用 `10.12.0.0/20` 作为沙箱网段。
- 宿主机到沙箱的通信应绕过代理。
- 沙箱内如需访问外部网络，可沿用宿主机相同的代理配置。

## 高并发配置建议

当宿主机需要承载较多并发沙箱实例时，建议提前提高文件句柄和网络队列相关限制。

### 1. 提高文件句柄上限

```bash
ulimit -n 65535
```

说明：

- 较高并发下，较低的文件句柄限制容易导致连接、socket 或设备句柄不足。
- 如需长期生效，建议结合 `limits.conf`、systemd `LimitNOFILE=` 或对应服务启动配置一并设置。

### 2. 提高内核网络接收队列长度

```bash
sysctl -w net.core.netdev_max_backlog=8192
```

说明：

- 该值应大于池化后每个网桥接入的 IP 个数。
- 如果该值过小，可能出现 ARP flooding，进一步导致 IP neighbor table learning failure。
- 除了提高 `net.core.netdev_max_backlog`，也可以通过 config 调整网桥配置，减少每个网桥对应的 IP 数目。
- 如需持久生效，建议写入 `/etc/sysctl.d/*.conf` 后执行 `sysctl --system`。

## 源码构建 EROFS 工具

如果发行版仓库中无法直接安装 `erofs-utils`，或软件源未提供该软件包，可通过源码方式构建安装。

### 1. 拉取源码并编译安装

```bash
git clone https://git.kernel.org/pub/scm/linux/kernel/git/xiang/erofs-utils.git
cd erofs-utils
./autogen.sh
./configure
make -j$(nproc)
make install
```

说明：

- `make install` 默认会将产物安装到系统路径，必要时可根据环境配合 `sudo` 使用。
- 如需自定义安装目录，可在 `./configure` 阶段传入 `--prefix=<path>`。
- 建议在安装完成后确认 `mkfs.erofs` 等命令已可用。
