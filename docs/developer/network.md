# Network 模块设计

Network 模块位于 `internal/netstack`，负责为 Sandbox 创建并复用隔离的网络环境。CNI 管理 Sandbox 对外网络，Conch 管理 network namespace、guest tap 和两层网络之间的地址转换。

## 1. 模块组成

| 组件 | 职责 |
| --- | --- |
| `Pool` | 预创建、分配、回收和补充 Network Slot |
| `Slot` | 保存 Slot ID、network namespace 路径、CNI IP、DNS 和 guest tap 地址 |
| `CNIManager` | 加载 CNI 配置，执行 CNI `ADD` / `DEL`，获取 Sandbox 对外 IP 和 DNS |
| `netns` | 创建并挂载独立的 network namespace |
| `guest_tap` | 创建 `tap0`，启用 IPv4 转发并配置 SNAT / DNAT |

`Pool` 拥有 Slot 的状态变化；Sandbox 和 VMM 只读取 Slot 提供的 namespace 路径、tap 名称和 CNI IP。

## 2. 网络结构

```text
CNI bridge / host network
          |
       veth pair
          |
  network namespace: slot-N
    ├── eth0：CNI 分配的对外 IP
    └── tap0：192.168.100.2/24
          |
      virtio-net
          |
  guest：192.168.100.21/24
```

每个 Slot 使用独立的 network namespace，因此可以复用相同的 guest 子网。Conch 在 namespace 内配置以下地址转换：

- guest 发出的流量以 CNI IP 作为源地址离开 namespace。
- 发往 CNI IP 的流量转发到 guest IP。

VMM 在 Slot 的 network namespace 中启动，并使用该 Slot 的 `tap0`。conch-init 通过初始化握手接收 guest IP、前缀长度、网关和 DNS，为第一个非 loopback 接口配置地址与默认路由，并更新 `/etc/resolv.conf`。

## 3. Network Slot

Network Slot 是一组可复用的网络资源，包括：

- Slot ID 和 `/run/conch/netns/slot-N` namespace。
- CNI 使用的 `conch-slot-N` container ID。
- CNI 创建的外层接口、分配的 IP 和返回的 DNS。
- Conch 创建的 `tap0`、IPv4 转发和 NAT 规则。

conchd 启动后并发填充 warm pool。创建 Sandbox 时，`Pool.Get` 取出一个 Slot 并绑定 Sandbox ID，同时触发后台补充；池为空时创建请求失败。Sandbox 删除后，`Pool.Release` 检查 namespace、CNI 接口和 tap 是否仍然存在：状态正常的 Slot 返回池中，状态异常的 Slot 被销毁并重新补充。

Slot 销毁时依次移除 tap 和 NAT、执行 CNI `DEL`、删除 network namespace，最后释放 Slot ID。CNI 清理遇到 `resource busy` 时会进行有限次数的退避重试。

## 4. CNI 边界

Network 模块通过 `containerd/go-cni` 加载 loopback 网络和一个默认 CNI 网络。CNI 负责 bridge/veth、IPAM、路由以及外层网络策略；Conch 不重复实现这些能力。

CNI Result 是 guest DNS 的唯一来源。Conch 对 CNI 返回的 DNS 进行校验、去重并最多保留 3 个 IPv4 nameserver，然后通过初始化协议下发给 conch-init；Conch 不读取 Host resolv.conf。

`plugin_conf_dir` 应指向 Conch 专用的 CNI 配置目录。`if_name` 默认为 `eth0`，并受 go-cni 的接口前缀规则约束：配置值必须是对应前缀生成的第一个接口，例如 `eth0`。

## 5. 配置

```yaml
network:
  warm_pool_size: 250
  cni:
    plugin_bin_dirs:
      - /usr/libexec/cni
    plugin_conf_dir: /etc/conch/cni/net.d
    if_name: eth0
```

- `warm_pool_size`：空闲 Slot 的目标数量，默认 250，最大 4000。
- `plugin_bin_dirs`：CNI 插件二进制目录。
- `plugin_conf_dir`：Conch 使用的 CNI 配置目录。
- `if_name`：CNI 在 namespace 中创建的接口名称。

## 6. 状态与退出

Slot ID、Slot 与 Sandbox 的绑定关系以及 CNI IP 只保存在内存中，Network Slot 本身不写入 state store。conchd 重启后会创建新的 Pool，不恢复或接管旧 Slot。启动时会在 warm pool 预热前扫描并删除 Conch 遗留的网络资源，再清理旧 Sandbox 状态记录并释放对应的 snapshot view；旧 Sandbox 不会恢复。

正常退出时，conchd 会先删除所有 Sandbox，再关闭 containerd host。host 关闭 Pool 时会停止后台补充，并尽力销毁队列中的空闲 Slot。单个 Slot 清理失败只记录日志，不阻止其余关闭流程。
