# Conch Netstack

#### 介绍
conch netstack 是为 Conch 配置主机侧网络资源的模块。本架构参考了 e2b-dev 项目，通过新建 netns、veth device 完成 Conch 虚机间的网络隔离，但针对 e2b-dev 复杂的网络模型，Conch netstack 采用网桥统一管理流量转发，并配置更简洁的 iptables 转发规则。另外，关键网络配置可通过配置文件配置。

#### 参考引用
1. 网络架构参考：https://github.com/e2b-dev/infra/tree/main/packages/orchestrator/internal/sandbox/network
