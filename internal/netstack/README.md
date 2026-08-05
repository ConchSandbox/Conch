# Conch 网络模块

Conch 网络模块的核心设计是以**槽位（slot）**为单元的网络池，由守护进程进行管理，在启动及删除沙箱时进行发放或回收。

- 每个槽位（slot）包括一整套可复用及提前预备好的网络状态：
  - 预设好的槽位ID，不跟随沙箱变动，如： `conch-slot-2`
  - Conch 专属的网络命名空间句柄，如： `/run/conch/netns/slot-2`
  - CNI 创建的沙箱外侧接口（对外IP地址、路由、IPAM 分配等），如： `eth0`
  - 虚拟机内侧 `tap` 接口及子网和IP地址
  - CNI IP 及虚拟机 tap IP 之间的转发规则

数值 Slot ID 是槽位的唯一身份来源；Pool 使用内存最小堆和占用位图分配 ID。Conch 每次启动都会创建新的内存分配器和 warm pool，不会从 BoltDB 恢复或接管旧 Slot。CNI ID 和 netns 路径均由 Slot ID 派生，不再分别保存可独立变化的 key/index。

网络资源的管理范围按照如下方案划分：
- Conch 负责槽位分配、沙箱生命周期、虚拟机 `tap0`、同命名空间内转发，以及 CNI 返回的外层 IP 与 tap 子网内 guest IP 之间的 NAT等。
- CNI 负责外层沙箱接口，通常是 `eth0`：网桥接入、veth 创建、IPAM、路由、插件返回的 DNS，以及插件管理的主机出口策略等。

模块内部按职责划分如下：
- `pool.go` 编排 Slot 的创建、健康检查、销毁、预填充和持续补池。
- `cni.go` 封装 CNI ADD、DEL 及外层接口检查。Conch 固定加载一个非 loopback CNI 配置；加上内置 loopback，CNI 的最小网络数固定为 2。
- `netns.go` 管理网络命名空间的创建、进入和删除。
- `guest_tap.go` 管理虚拟机侧 `tap0`、IPv4 转发和 NAT。
- `slot.go` 保存 Slot 状态和地址派生。状态修改仅限 `netstack` 包内部；Sandbox 只通过 `Slot` 的只读方法获取 ID、CNI IP、netns 路径和 tap 名称。
- `slot` 子包只提供 ID 分配器和 warm queue，不包含网络领域状态，也不执行 CNI、netlink 或 iptables 操作。


## 设计来源

网络池化、可复用槽位、网络命名空间、生命周期管理、以及虚拟机侧 tap/NAT 模型，参考了 [E2B 沙箱网络](https://github.com/e2b-dev/infra/tree/main/packages/orchestrator/internal/sandbox/network)的设计。

在此基础上的主要变化是 Sandbox/CNI 职责拆分：Conch 不再直接创建外层网络栈，外层沙箱网络边界现在交给 CNI 插件处理，而 Conch 继续负责 VM 边界以内的创建及管理。

宿主机配置需求、CNI 配置、运行流程以及手动验证步骤可参考 [docs/guide/net_usage.md](../../docs/guide/net_usage.md)。
