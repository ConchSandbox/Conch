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

`Pool` 初始化时会查找主机 `0.0.0.0/0` IPv4 默认路由，并为 CNI bridge 配置主机转发规则。因此，运行 `conchd` 的主机必须存在类似 `default via 192.168.1.1 dev eth0` 的可用路由；否则网络池初始化失败，`conchd` 不会进入可用状态。

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

Slot 销毁时依次移除 tap 和 NAT、执行一次 CNI `DEL`、删除 network namespace，最后释放 Slot ID。CNI `DEL` 失败时保留 network namespace 和 Slot ID 并返回错误；启动 cache reconciliation 失败时保留 cache 并终止启动。

## 4. CNI 边界

Network 模块直接通过 `libcni` 加载一个默认 CNI 网络。ADD、DEL 与启动 cache reconciliation 共用同一个 `libcni.CNIConfig`，确保插件配置和 cache root 一致。CNI 负责 bridge/veth、IPAM、路由以及外层网络策略；Conch 通过 netlink 将 namespace 中的 loopback 接口置为 UP，不再调用 loopback CNI 插件。

CNI Result 是 guest DNS 的唯一来源。Conch 对 CNI 返回的 DNS 进行校验、去重并最多保留 3 个 IPv4 nameserver，然后通过初始化协议下发给 conch-init；Conch 不读取 Host resolv.conf。

`plugin_conf_dir` 应指向 Conch 专用的 CNI 配置目录。外层接口名固定为内部实现值 `eth0`；libcni cache root 固定为 `/var/lib/conch/cni`，result cache 实际位于其 `results` 子目录。这两个值不属于用户配置接口。

libcni cache root 不控制 CNI 插件自身的持久化目录。仓库提供的 host-local 配置另外将 `dataDir` 设置为 `/var/lib/conch/cni/networks`，使默认的 result cache 与 IPAM 租约都位于 Conch 专用目录。

默认 bridge CNI 配置必须提供顶层 IPv4 `ipam.subnet`。conchd 启动时会检查该子网是否与 Host 的接口网段或非默认路由重叠；配置所指定 bridge 自身的网段不参与检查。发现冲突或无法完成检查时，CNI 初始化失败，conchd 不会启动网络池。

## 5. 配置

```yaml
network:
  warm_pool_size: 250
  cni:
    plugin_bin_dirs:
      - /usr/libexec/cni
    plugin_conf_dir: /etc/conch/cni/net.d
```

- `warm_pool_size`：空闲 Slot 的目标数量，默认 250，最大 4000。
- `plugin_bin_dirs`：CNI 插件二进制目录。
- `plugin_conf_dir`：Conch 使用的 CNI 配置目录。

## 6. 状态与退出

Slot ID、Slot 与 Sandbox 的绑定关系以及 CNI IP 只保存在内存中，Network Slot 本身不写入 state store。conchd 重启后会创建新的 Pool，不恢复或接管旧 Slot。启动时会在 warm pool 预热前先清理仍挂载的旧 network namespace，再通过 libcni 枚举配置 cache root 中的 current-format attachment，并对已经失去 Slot 状态的 `conch-slot-*` attachment 执行 CNI `DEL`。随后清理旧 Sandbox 状态记录并释放对应的 snapshot view；旧 Sandbox 不会恢复。

正常退出时，conchd 会先删除所有 Sandbox，再关闭 containerd host。host 关闭 Pool 时会停止后台补充，并尽力销毁队列中的空闲 Slot。单个 Slot 清理失败只记录日志，不阻止其余关闭流程。
