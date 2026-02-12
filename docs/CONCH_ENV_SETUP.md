# conch-env-setup.sh 脚本使用说明

## 概述

本脚本（conch-env-setup.sh）是 Conch 项目的一键环境搭建与流程执行工具，旨在自动化完成 Conch 项目运行所需的全流程操作，包括运行时依赖安装、镜像仓库配置、容器化离线构建、目标镜像解包分析等核心步骤。
脚本具备「分步执行」「全流程一键执行」「自定义镜像版本」等特性，可灵活适配开发、测试等不同场景，有效降低环境搭建复杂度，避免手动操作失误。

## 脚本基础信息
- 脚本路径：Conch 项目根目录下的 `scripts/conch-env-setup.sh`（必须在项目根目录执行，否则会导致路径错误）
- 脚本作用：自动化完成 Conch 项目运行环境部署、编译构建、镜像处理全流程
- 默认配置：内置默认镜像地址、依赖版本，无需额外配置即可快速使用

## 使用前提
在执行脚本前，请确保系统满足以下条件，否则可能导致脚本执行失败：
1. 系统要求：支持 openEuler（推荐）、CentOS、Ubuntu 等主流 Linux 发行版，仅适配 x86_64 架构
2. 前置工具：系统已安装 `git`、`make`、`wget`、`tar` 基础工具（可通过 `yum install -y git make wget tar` 或 `apt-get install -y git make wget tar` 快速安装）
3. 网络要求：能够正常访问 GitHub（用于下载 containerd、cloud-hypervisor 依赖）和 hub.oepkgs.net（用于拉取 Conch 项目镜像）
4. 目录要求：已克隆 Conch 项目代码并进入根目录（脚本依赖项目根目录路径进行文件挂载、编译输出）


## 核心使用语法
脚本的核心使用格式统一为：
./scripts/conch-env-setup.sh [COMMAND] [OPTIONS]

说明：
- COMMAND：必填，指定脚本执行的核心命令（如安装依赖、拉取镜像、编译构建等）
- OPTIONS：可选，用于自定义镜像版本等配置（如 --build_image、--main_image）

### a. 核心命令说明

脚本支持 6 个核心命令，可根据实际需求选择「分步执行」或「全流程执行」，各命令功能、使用示例如下：

#### 1. install 命令
功能：检查并安装 Conch 项目运行所需的核心依赖（containerd + cloud-hypervisor），若依赖已安装则自动跳过，避免重复安装。

依赖版本：
- containerd：默认安装 v2.2.1（自动配置系统服务，设置开机自启）
- cloud-hypervisor：默认安装 v48.0（静态二进制文件，安装至 /usr/local/bin）

使用示例：
```
./scripts/conch-env-setup.sh install
```

执行逻辑：
- 先检测系统是否已安装 cloud-hypervisor，未安装则自动下载并配置执行权限
- 再检测系统是否已安装 containerd，未安装则下载压缩包、解压安装、配置系统服务、启动并设置开机自启
- 依赖安装完成后，更新系统命令哈希表，确保后续命令可正常调用

#### 2. pull 命令
功能：拉取 Conch 项目所需的构建镜像和功能镜像。
默认镜像：
- 构建镜像（build_image）：hub.oepkgs.net/conch/conch-builder:v0.1
- 功能镜像（main_image）：hub.oepkgs.net/conch/conch-index:v0.1

使用示例（默认镜像）：
```
./scripts/conch-env-setup.sh pull
```

执行逻辑：
- 自动解析镜像地址中的域名，为每个域名创建 containerd 证书配置目录（/etc/containerd/certs.d/[域名]）
- 生成 hosts.toml 配置文件，支持镜像拉取和解析功能
- 使用 ctr 命令拉取指定的构建镜像和功能镜像

#### 3. build 命令

功能：在容器内执行 Conch 项目的离线编译，避免污染宿主机环境，编译完成后会在项目根目录的 `./bin` 目录下生成可执行工具（如 conch-unpack）。
核心特性：容器化编译，宿主机仅需提供项目目录挂载，无需安装 Go 等编译依赖。

使用示例：
```
./scripts/conch-env-setup.sh build
```
执行逻辑：
- 使用 ctr 命令启动构建容器，将宿主机的 Conch 项目根目录挂载至容器内的 /build 目录（读写权限）
- 设置容器内的 GOPATH 环境变量，确保编译正常执行
- 在容器内执行 `make build-offline` 命令完成离线编译，编译产物输出至宿主机的 ./bin 目录
- 编译完成后自动删除容器，避免残留临时容器

#### 4. process 命令

功能：使用 `build` 命令编译生成的 `./bin/conch-unpack` 工具，对指定的功能镜像进行解包和初步分析。

前置条件：必须先执行 `build` 命令完成编译，否则会提示 `./bin/conch-unpack` 不存在。

使用示例：
```
./scripts/conch-env-setup.sh process
```
执行逻辑：
- 检测项目根目录下的 ./bin/conch-unpack 是否存在且具备执行权限
- 若存在则调用该工具，传入功能镜像（main_image）地址进行解包分析
- 若不存在则输出错误提示，引导用户先执行 build 命令

#### 5. all 命令（推荐）

功能：一键执行全流程操作，按顺序自动运行 `install → pull → build → process` 四个命令，无需手动分步执行，适合首次搭建环境或快速部署场景。

核心优势：全程自动化，无需人工干预，减少操作步骤和失误概率。

使用示例：
```
./scripts/conch-env-setup.sh all
```
执行逻辑：

- 依次执行 install（安装依赖）→ pull（拉取镜像）→ build（容器化编译）→ process（镜像解包）
- 前一个步骤执行成功后才会进入下一个步骤，若某一步骤失败则终止后续执行，并输出错误提示

#### 6. help 命令

功能：查看脚本的核心命令、使用语法和自定义参数说明，适合忘记命令或参数时快速查询。

使用示例：
```
./scripts/conch-env-setup.sh help
```
输出内容：会展示脚本的使用语法、所有核心命令说明、自定义参数说明，与本 README 核心内容一致。

### b. 自定义参数配置

脚本支持通过自定义参数覆盖默认的镜像地址和标签，适配不同版本的镜像需求，参数需跟在核心命令之后使用，格式为 `--参数名=值`。

支持的自定义参数如下：

#### 1. --build_image 参数

功能：指定 Conch 项目的构建镜像地址和标签，替换默认的 build_image。
默认值：hub.oepkgs.net/conch/conch-builder:v0.1

使用示例：

```
#拉取自定义版本的构建镜像
./scripts/conch-env-setup.sh pull --build_image=hub.oepkgs.net/conch/conch-builder:v0.2

# 一键全流程，使用自定义构建镜像执行编译
./scripts/conch-env-setup.sh all --build_image=my-registry/conch-builder:latest
```

#### 2. --main_image 参数

功能：指定 Conch 项目的功能/分析镜像地址和标签，替换默认的 main_image。
默认值：hub.oepkgs.net/conch/conch-index:v0.1

使用示例：
```
# 拉取自定义版本的功能镜像
./scripts/conch-env-setup.sh pull --main_image=hub.oepkgs.net/conch/conch-index:v0.2

# 执行镜像解包，使用自定义功能镜像
./scripts/conch-env-setup.sh process --main_image=my-registry/conch-index:latest

```
#### 3. 参数使用说明
- 参数可单独使用，也可组合使用（同时自定义 build_image 和 main_image）
- 参数值需填写完整的镜像地址（包含域名、仓库名、标签），否则会导致镜像拉取失败
- 自定义的镜像需确保可正常拉取（仓库地址可访问、标签存在）

组合参数使用示例：
```
./scripts/conch-env-setup.sh all --build_image=hub.oepkgs.net/conch/conch-builder:v0.2 --main_image=hub.oepkgs.net/conch/conch-index:v0.2
```
## 常见问题与解决方案

以下是脚本执行过程中常见的问题及对应的解决方案，若遇到其他问题可先检查日志输出（脚本会实时打印执行日志）。

问题 1：下载 containerd/cloud-hypervisor 失败

报错示例：Error: Failed to download cloud-hypervisor. 或 wget: unable to resolve host address ‘github.com’

解决方案：

- 检查网络是否正常，能否访问 GitHub（可通过 ping github.com 测试）
- 若网络受限，可手动下载对应版本的依赖包至 Conch 项目根目录，重新执行脚本（脚本会检测本地文件，跳过下载步骤）
- 手动下载地址：
  - containerd v2.2.1：https://github.com/containerd/containerd/releases/download/v2.2.1/containerd-2.2.1-linux-amd64.tar.gz
  - cloud-hypervisor v48.0：https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v48.0/cloud-hypervisor-static

问题 2：执行 process 命令提示「./bin/conch-unpack 不存在」

报错示例：Error: ./bin/conch-unpack executable not found.

解决方案：先执行 `build` 命令完成容器化编译，生成 conch-unpack 工具后再执行 process 命令，示例：
```
./scripts/conch-env-setup.sh build
./scripts/conch-env-setup.sh process
```

问题 3：拉取镜像失败（提示证书验证失败或无法连接仓库）

报错示例：x509: certificate signed by unknown authority 或 failed to connect to registry

解决方案：
- 确认镜像仓库地址正确（自定义镜像时检查域名、仓库名、标签是否正确）
- 检查 containerd 服务是否正常运行（执行 `systemctl status containerd`，若未运行则执行 `systemctl start containerd`）
- 脚本已自动配置 SSL 跳过验证，无需额外配置证书，若仍失败可手动删除 /etc/containerd/certs.d/[镜像域名] 目录后重新执行 pull 命令