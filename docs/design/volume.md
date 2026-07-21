# Conch Volume Design

## 1. 设计目标

Conch volume 提供面向 sandbox 的 host 目录挂载能力：用户在创建 sandbox 时声明若干
host 目录（host-path），Conch 把它们挂进 guest 内指定路径。

设计目标：

- 支持挂卷，将host指定目录（绝对路径）挂载到guest指定目录（绝对路径）。
- 后端可替换，当前实现 virtiofs，后续可扩展其他共享文件系统或 VMM 原生挂载后端。


依赖组件：

- virtiofsd: v1.13.3+
- stratovirt: v2.5.0+, with vhost-user fs feature

## 2. 组件架构

### 2.1 架构示意图

```text
Python SDK / conch CLI
        |
        v
conchd HTTP API
        |
        v
Sandbox Manager
        |
        +-----------------------------------+
        |                                   |
        v                                   v
Volume Mount Resolver              Shared Dir Backend (virtiofs)
        |                                   |
        v                                   v
/run/conch/sandboxes/<id>/volume/   virtiofsd process/socket (×1)
   ├─ 0/      (bind: host-path A)           |
   ├─ 1/      (bind: host-path B)           v
   └─ config.json                    VMM Adapter
        |                                   |
        v                                   v
   guest agent (passive)            stratovirt vhost-user-fs-pci (×1)
        |
        v
   mount --bind per config.json entry
```

`Sandbox Manager` 负责校验 `volume_mounts`（数量 ≤ max_mounts、source 绝对路径且存在、
path 绝对路径与系统路径保护、path 去重），协调 volume dir 与 VMM 启动顺序，并在 sandbox
删除或创建失败时清理。

`Volume Mount Resolver` 把 `volume_mounts` 解析为 host 侧 bind 计划
（`source` → `<runtime>/volume/<index>`）与 guest 侧 `config.json` 条目
（`index` → `path` + `readonly`）。

`Shared Dir Backend` 定义可替换后端边界。当前 virtiofs 后端负责：创建 per-sandbox
runtime 目录与 `volume` 子目录、执行 host 侧 `mount --bind`、写 `config.json`、启动
单个 virtiofsd、等待 socket ready、生成 VMM 所需 socket/tag，并在 sandbox 删除或创建
失败时清理 virtiofsd 与 bind。

`VMM Adapter` 把后端返回的 virtiofs 设备转换为具体 VMM 参数。当前 stratovirt 使用
**单个** `vhost-user-fs-pci`，并在 kernel cmdline 追加极小 sharefs 开关。

`guest agent` 开机检测 cmdline 中 `conch.sharefs=virtiofs` 开关；存在则挂载 virtiofs 到
固定 guest 路径、读取 `<mp>/config.json`、对每条挂载 `mkdir -p` 目标并 `mount --bind`；
不存在则跳过整个挂载分支。agent 始终是被动方。


### 2.2 实现原则与约束

- 挂载信息通过共享目录内的 `config.json` 传递给 guest。
- 信息单向从conchd发给guest，guest 被动响应，不主动向 conchd 请求挂载信息。
- 挂载通过**单 virtiofsd共享目录**实现——一个 sandbox 最多起一个
  virtiofsd、一个 `vhost-user-fs` 设备，所有挂载是该共享目录下的子路径，
  guest agent 通过共享目录内的 `config.json` 得到子路径→目标路径映射并
  执行 `mount --bind`。
- conchd 重启处理：
  - 在运行VMM：attach 现有 VMM 进程；若该 sandbox 带挂载，则复用现存的
    host bind（内核 bind mount）与 virtiofsd 进程，重建 volume dir 管
    理态并注册清理回调。
  - 已退出VMM： VMM 已退出而 virtiofsd/bind 残留，清理残留 virtiofsd
    进程、socket 与 runtime目录。
- 一致性：virtiofsd 1.13.x（Rust 版）没有 cache 参数；缓存模式是
  **guest 侧** mount选项。agent 挂 virtiofs 时用默认 cache 模式
  （`mount -t virtiofs conchfs ...`，不带 `-o cache=...`）。实测在本
  guest 内核 + Stratovirt 组合上 `-o cache=none`会让挂载卡死，故不使用。
  多沙箱绑同一 host-path 的并发写 POSIX 一致性仍不保证（无集群 FS）。


实现须封装 virtiofsd 参数生成，便于适配不同发行版差异。后端启动后等待 socket 文件
出现并可连接再返回给 VMM adapter。

### 2.3 virtiofs 后端（单进程模型）

每个 sandbox 最多一个 virtiofsd 进程，仅在 `volume_mounts` 非空时启动。目录布局：

```text
/run/conch/sandboxes/<sandbox-id>/
  ├─ volume/                # virtiofsd --shared-dir 导出根
  │   ├─ 0                  # bind: mounts[0].source
  │   ├─ 1                  # bind: mounts[1].source
  │   └─ config.json        # 挂载信息（agent 读取）
  └─ virtiofs.sock          # vhost-user-fs socket
```

guest 侧固定常量：

```text
tag              = conchfs
guest mountpoint = /run/conch/volume
```

virtiofsd 启动参数：

```bash
virtiofsd --socket-path <runtime>/virtiofs.sock --shared-dir <runtime>/volume
```

### 2.4 stratovirt 适配

本环境 stratovirt 2.5.0 支持 vhost-user-fs：

```text
-device vhost-user-fs-pci,id=<device_id>,chardev=<chardev_id>,tag=<mount_tag>
```

无论多少挂载，只追加**一组** chardev + device：

```bash
-chardev socket,id=charfs0,path=/run/conch/sandboxes/<id>/virtiofs.sock \
-device vhost-user-fs-pci,id=fs0,chardev=charfs0,tag=conchfs,bus=pcie.0,addr=0x14
```

- 仅当 `volume_mounts` 非空时追加该组设备，并给 `-machine` 加 `mem-share=on`
  （vhost-user-fs 要求 guest 内存与 virtiofsd 共享）；无挂载时两者都不加。
- PCI 地址固定 `0x14`，避开 virtio-pmem 的 `0x12..` 段；单设备不再受槽位限制。
- kernel cmdline 仅追加 ` conch.sharefs=virtiofs`，**不**带任何卷表，无挂载时不追加。

### 2.5 挂载信息传递：config.json

挂载信息通过共享目录内的 `config.json` 传递给 guest，不通过 kernel cmdline 卷表、不
通过 agent RPC。host 在启动 sandbox 前（VM 起来前）把 `config.json` 写入
`<runtime>/volume/config.json`，该文件随 virtiofs 暴露到 guest 的
`/run/conch/volume/config.json`。

`config.json` schema：

```json
{
  "version": 1,
  "mounts": [
    {"index": 0, "path": "/workspace", "readonly": false},
    {"index": 1, "path": "/data",     "readonly": true}
  ]
}
```

字段：

```text
version: int          schema 版本，当前 1。
mounts[].index: int   对应 <runtime>/volume/<index> 子路径；agent bind 源 = <guest-mp>/<index>。
mounts[].path: str    guest 内目标绝对路径，agent mkdir -p 后 bind。
mounts[].readonly: bool 默认 false。
```

## 3. 接口设计

### 3.1 配置文件

`volume` 配置段：

```yaml
volume:
  max_mounts: 10
  backend: virtiofs
  virtiofs:
    binary: /usr/libexec/virtiofsd
    runtime_dir: /run/conch/sandboxes
```

字段：

```text
volume.max_mounts
  单 sandbox 最大挂载数。纯策略上限（单 virtiofs 模型下不受 PCI/cmdline 限制）。
  默认值：10

volume.backend
  后端名称。默认 virtiofs。

volume.virtiofs.binary
  virtiofsd 二进制路径。可为可执行文件名（走 PATH 查找）或绝对路径。
  默认值：virtiofsd
  注意：Debian/Ubuntu 的 virtiofsd 包把二进制装在 /usr/libexec/virtiofsd，不在默认
  PATH 中，此时必须配成绝对路径，否则 exec.LookPath 失败，挂卷时报
  "executable file not found in $PATH"。

volume.virtiofs.runtime_dir
  per-sandbox runtime 目录的父目录，实际目录 = <runtime_dir>/<sandbox-id>/。
  默认值：/run/conch/sandboxes
```

其他重要配置说明：
1. Guest 内核配置：需开启 CONFIG_VIRTIO_FS 和 CONFIG_FUSE_FS 且为 =y（内建），因为沙箱
   initrd 是极简 Alpine（无 modprobe），且镜像 rootfs 通常不带 /lib/modules；编成 =m 时
   virtiofs.ko 无法加载，guest 内 `mount -t virtiofs` 报 "unknown filesystem type
   'virtiofs'"。Conch 用到的其它 guest 内核选项（EROFS_FS、OVERLAY_FS、VIRTIO_NET、
   VIRTIO_PMEM、VIRTIO_VSOCKETS 等）同样建议 =y。
2. StratoVirt：二进制需带 vhost-user-fs 特性。可以通过
   `strings /usr/local/bin/stratovirt | grep -i "virtiofs"` 判断。
3. 共享内存：挂卷时 Conch 自动给 `-machine` 加 `mem-share=on`。vhost-user-fs 需要 guest
   内存与 virtiofsd 后端进程共享，否则 StratoVirt 报 "the memory must be shared"。
4. PCI 地址：单 `vhost-user-fs-pci` 固定 `bus=pcie.0,addr=0x14`，避开 virtio-pmem 占用的
   `0x12..` 段；StratoVirt 对该设备要求显式 bus 与 addr。

### 3.2 SDK 接口

#### Sandbox.create

SDK 对外参数使用 Python 风格 `volume_mounts`，请求到 conchd 时转换为 `volumeMounts`。

```python
from conch import Sandbox

sandbox = Sandbox.create(
    template_id="tmpl_8f3a2c1b9d4e6a7f0c2b5d8e1a4f7c9d",
    namespace="team-a",
    volume_mounts=[
        {"source": "/host/path/cache",   "path": "/mnt/cache"},
        {"source": "/host/path/dataset", "path": "/data", "readonly": True},
    ],
)
```

`volume_mounts` 类型：

```python
list[dict[str, object]]
```

每个元素字段：

```text
source: str
  host 绝对路径，必须已存在。conchd 不做路径白/黑名单限制，信任 conchd 鉴权与部署隔离。

path: str
  guest 内挂载目标绝对路径。

readonly: bool
  是否只读 bind。默认 false。
```

SDK 发送 payload：

```json
{
  "namespace": "team-a",
  "template_id": "tmpl_8f3a2c1b9d4e6a7f0c2b5d8e1a4f7c9d",
  "volumeMounts": [
    {"source": "/host/path/cache",   "path": "/mnt/cache", "readonly": false},
    {"source": "/host/path/dataset", "path": "/data",      "readonly": true}
  ]
}
```

## 4. 约束和限制

- 规格：单 sandbox 最多 `volume.max_mounts`（默认 10）个挂载。
- guest内使用绝对路径，且不允许是 `/`、`/proc`、`/sys`、`/dev`、`/run` 等系统
  关键路径或其子路径。
- 同一 sandbox 内 `path` 不允许重复。
- 挂卷与快照互斥：带挂载的 sandbox 不支持 snapshot start、pause、resume、export；相关
  调用直接报错。
- 用户 host-path 不随 sandbox 删除而删除。

