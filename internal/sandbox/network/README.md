# Conch Network

#### 介绍
conch network是为conch配置网络。本架构参考了e2b-dev项目，通过新建netns，veth device完成conch虚机间的网络隔离，但针对e2b-dev复杂的网络模型，conch network采用了网桥统一管理流量转发，配置更简洁的iptables转发规则，另外关键网络配置可通过配置文件配置等一系列方式优化了网络模型。

#### 参考引用
1.  网络架构参考：https://github.com/e2b-dev/infra/tree/main/packages/orchestrator/internal/sandbox/network
