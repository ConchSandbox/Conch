# Conch 网络使用说明

此篇文档将基于当前的 Conch 网络设计讲述如何配置并验证，主要面向单机 Linux 开发环境，不依赖 Kubernetes。

## 基本设计

- Conch 创建可复用网络槽位和网络命名空间。
- Conch 为每个槽位使用专属的 CNI ID 调用 CNI `ADD`，例如 `conch-slot-2`，而非按照 CRI 的惯例使用临时生成的沙箱ID。
- CNI 创建外层沙箱接口，通常是 `eth0`，包括 bridge/veth、IPAM、路由以及策略等。
- Conch 随后创建虚拟机侧 `tap`，在内部启用转发，并在对外 IP 与 tap 子网内 IP 之间安装本地转发规则。
- 在显式删除槽位时，Conch 先移除 tap/NAT，再调用 CNI `DEL`，删除网络命名空间，最后根据 CNI 配置移除 Conch 拥有的空 CNI bridge link。

## 宿主机准备

- 需要以 root 权限运行 `conchd`，或提供等价的 `CAP_SYS_ADMIN` 与 `CAP_NET_ADMIN` 能力。
- 在 `/opt/cni/bin` 或设置的 `network.cni.plugin_bin_dirs`下安装 CNI 插件二进制文件： `bridge`、`host-local` 和 `loopback` 。
- 将 Conch CNI 配置放在 Conch 专用配置目录中。默认目录为 `/etc/conch/cni/net.d`。

仓库内提供了示例配置 `config/cni/net.d/10-conch.conf`。如果 `network.cni.plugin_conf_dir` 保持默认值，且 `/etc/conch/cni/net.d` 中没有 CNI 配置文件，Conch 会回退到已加载 Conch 配置文件旁边的 `cni/net.d`。使用仓库内的 `config/config.yaml` 时，该回退路径即为 `config/cni/net.d`。

## CNI 配置示例

仓库内示例是一个 bridge 风格的 CNI 配置：

```json
{
  "cniVersion": "1.0.0",
  "name": "conch-bridge",
  "type": "bridge",
  "bridge": "cni-conch0",
  "isGateway": true,
  "ipMasq": true,
  "ipam": {
    "type": "host-local",
    "subnet": "10.12.0.0/20",
    "routes": [{ "dst": "0.0.0.0/0" }]
  }
}
```

其中 `bridge` 字段需要显式配置，用于在清理资源时找到对应的 CNI bridge link 进行删除。不要让 Conch 指向同时包含其他 runtime CNI 配置的目录。

## Conch 网络配置示例

`config/config.yaml` 中与网络相关的配置如下：

```yaml
network:
  pool_size: 250
  dynamic_reservation: false
  # bridge_count: 1 # deprecated for the current CNI path
  tap_ip: 192.168.100.2
  tap_mask: 24
  cni:
    plugin_bin_dirs:
      - /opt/cni/bin
    plugin_conf_dir: /etc/conch/cni/net.d
    plugin_max_conf: 1
    if_name: eth0
    setup_serially: false
```

Conch 网络配置需要注意如下事项：
- `pool_size` 给定了网络池的槽位数目，以及宿主机上可同时运行的沙箱最大数目。
- `tap_ip` 和 `tap_mask` 给定了每个沙箱内部面向虚拟机的 `tap` 子网。
- `plugin_bin_dirs` 指向 CNI 插件目录, `plugin_conf_dir` 指向 Conch CNI 配置目录。
- `if_name` 给定了 CNI 创建的沙箱网络接口名称, 通常是 `eth0`。


## 手动验证

以 root 身份启动 `conchd`，并验证网络池预填充：

```bash
sudo ./bin/conchd -config config/config.yaml
ip netns list
```

- 预期结果：可以看到用于预填充 slot 的 namespace，例如 `ns-2`。

检查其中一个 namespace：

```bash
sudo ip netns exec ns-2 ip addr show eth0
sudo ip netns exec ns-2 ip route
sudo ip netns exec ns-2 ip addr show tap0
sudo ip netns exec ns-2 sysctl net.ipv4.ip_forward
sudo ip netns exec ns-2 iptables -t nat -S
```

- 预期结果：
  - `eth0` 处于 **up** 状态，并持有 CNI 子网中的 IP，例如 `10.12.0.2/20`。
  - 默认路由指向 CNI bridge gateway，例如 `10.12.0.1`。
  - `tap0` 存在，并持有配置中的 tap IP，例如 `192.168.100.2/24`。
  - `net.ipv4.ip_forward = 1`。
  - namespace-local NAT 规则将外层 CNI IP 映射到 tap 子网内 guest IP，例如 `192.168.100.21`。


检查宿主机 bridge 和 CNI IPAM 状态：

```bash
ip link show cni-conch0
ls /var/lib/cni/networks/conch-bridge
```

- 预期结果：slot 存在时 bridge 处于 up 状态，host-local IPAM 目录中有一个对应每个 CNI 分配 IP 的 allocation 文件。`last_reserved_ip.*` 和 `lock` 是 host-local 的元数据，不是泄漏的 slot allocation。


conchd 正常关闭后：

```bash
ip netns list
ip link show cni-conch0
ip route show table main | grep 10.12
ip neigh show | grep 10.12
ls /var/lib/cni/networks/conch-bridge
```

- 预期结果：正常退出（包括 `SIGTERM`, `SIGINT` 等）会保留网络资源，以便重启后重新接管。netns、CNI IPAM 分发文件以及 CNI bridge 都可能继续存在。重启 `conchd` 后，保留下来的预热槽位应被接管，而已分配给沙箱的应根据沙箱的具体状态进行恢复。


## 手动清理流程

正常退出 `conchd` 会保留所有的网络资源，如果需要完全重置（即不保留任何沙箱、快照、网络等记录），可在退出 `conchd` 后删除数据记录 `/var/lib/conch/state.db`，并使用如下指令手动删除命名空间、网桥以及 IPAM 目录：

```bash
sudo ip netns delete ns-X
sudo ip link delete cni-conch0 2>/dev/null || true
sudo rm -rf /var/lib/cni/networks/conch-bridge
```

如果 `iptables -t nat -S | grep CNI` 仍能看到 CNI 链，先删除引用这些链的 jump 规则，再清空并删除空链；仅删除 IPAM 目录不会清理 iptables 规则。
