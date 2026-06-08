# Conch 网络模块

Conch 网络模块的核心设计是以**槽位（slot）**为单元的网络池，由守护进程进行管理，在启动及删除沙箱时进行发放或回收。

- 每个槽位（slot）包括一整套可复用及提前预备好的网络状态：
  - 预设好的槽位ID，不跟随沙箱变动，如： `conch-slot-2`
  - 预留的网络命名空间，如： `/var/run/netns/ns-2`
  - CNI 创建的沙箱外侧接口（对外IP地址、路由、IPAM 分配等），如： `eth0`
  - 虚拟机内侧 `tap` 接口及子网和IP地址
  - CNI IP 及虚拟机 tap IP 之间的转发规则

网络资源的管理范围按照如下方案划分：
- Conch 负责槽位分配、沙箱生命周期、虚拟机 `tap0`、同命名空间内转发，以及 CNI 返回的外层 IP 与 tap 子网内 guest IP 之间的 NAT等。
- CNI 负责外层沙箱接口，通常是 `eth0`：网桥接入、veth 创建、IPAM、路由、插件返回的 DNS，以及插件管理的主机出口策略等。


## 设计来源

网络池化、可复用槽位、网络命名空间、生命周期管理、以及虚拟机侧 tap/NAT 模型，参考了 [E2B 沙箱网络](https://github.com/e2b-dev/infra/tree/main/packages/orchestrator/internal/sandbox/network)的设计。

在此基础上的主要变化是基于 CRI/CNI 兼容所需的职责拆分：Conch 不再直接创建外层网络栈，外层沙箱网络边界现在交给 CNI 插件处理，而 Conch 继续负责 VM 边界以内的创建及管理。

宿主机配置需求、CNI 配置、运行流程以及手动验证步骤可参考 [docs/guide/net_usage.md](../../docs/guide/net_usage.md)。
