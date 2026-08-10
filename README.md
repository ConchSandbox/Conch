<img src="./docs/assets/Conch-logo.jpg" alt="Conch logo" style="width:200px;" />

<a href="https://atomgit.com/openeuler/Conch.git"><img src="https://img.shields.io/badge/atomgit-Conch-blue"/></a> ![license](https://img.shields.io/badge/license-Mulan%20PSL%20v2-blue) <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26+-blue"/> </a><a href="https://www.python.org/"><img src="https://img.shields.io/badge/Python-SDK-blue"/> </a>

# Conch - Agent Sandbox Engine


Conch 是一个基于 Go 开发的容器沙箱引擎，能够适用于 Agent 对沙箱的高启动性能、高弹性、高 I/O 性能和高密度部署的诉求。
项目围绕以下 Agent 对沙箱新需求展开：
1. 新生态：相比传统命令行和K8S云原生生态，提供 Agent 原生的沙箱管理 API 和 SDK；
2. 新镜像：相对传统 OCI v1 容器镜像格式，提供 EROFS 镜像格式，统一管理容器镜像和快照；
3. 新硬件（超节点）：相比传统单机管理容器镜像，利用超节点高速互联能力，提供跨级镜像共享和管理机制。

## 核心特性

- 轻量安全隔离 -- 支持虚拟沙箱，对 Agent 任务进行安全隔离。支持完整的生命周期管理，包括创建、暂停、恢复和删除等操作。
- 快照启动加速 -- 支持虚拟机内存和根文件系统的快照功能。通过快照机制，可以实现秒级的沙箱启动，显著提升大规模部署场景下的资源利用效率。快照采用写时复制（Copy-on-Write）技术，最小化存储开销。
- 精简容器网络 -- 通过 CNI 插件管理沙箱网络命名空间的外层网络，同时由 Conch 保留可复用 slot、netns 生命周期、VM guest tap 和 guest NAT 的管理权，在保持网络池化低时延的同时明确外层网络边界。

## 文档

- [快速开始](docs/user/getting-started.md)
- [环境准备](docs/user/environment-setup.md)
- [RPM 安装](docs/user/rpm-install.md)
- [Template 与镜像](docs/user/template.md)
- [Python SDK](docs/user/python-sdk.md)

其他文档见 [Conch 文档导航](docs/README.md)。

## 许可证

木兰宽松许可证， 第2版

## 贡献指南

欢迎社区贡献代码和文档。
