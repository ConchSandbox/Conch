# Conch 文档

## 用户

- [快速开始](user/getting-started.md)：从源码编译、启动 Conch 并执行基本操作。
- [环境准备](user/environment-setup.md)：安装源码构建与运行依赖。
- [RPM 安装与服务管理](user/rpm-install.md)：通过 RPM 安装并使用 systemd 管理 Conch。
- [Template 与镜像](user/template.md)：使用 CLI 创建、发布和管理 Template 与镜像。
- [Python SDK](user/python-sdk.md)：使用 Python 创建和操作 Sandbox。
- [VSOCK 通信](user/vsock.md)：从 guest 连接 StratoVirt host 上的 VSOCK 服务。

## Conch 开发

- [Template 模块接口](developer/template-interface.md)：Template 数据模型和 Store 接口。
- [Network 模块设计](developer/network.md)：Network Slot、CNI、guest tap 和网络生命周期。
- [Volume 设计](developer/volume.md)：virtiofs、VMM 与 guest agent 的卷架构。
- [conch-init Agent API](developer/conch-init-api.md)：sandbox 内 Agent 的 Connect RPC 接口。
- [架构图源文件](developer/arch.drawio)：可编辑的 draw.io 架构图。
