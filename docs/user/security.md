# 安全说明

本文档介绍 Conch 的安全模型、部署加固要求，以及使用中需要注意的既有行为。

## 安全模型

Conch 的信任边界自上而下分为三层：

```
 ┌───── 宿主机 / 可信计算基 TCB（必须假定可信）─────┐
 │ root · conchd · /etc/conch/* · PATH 中的可执行文件 │
 │ CNI 插件目录 · containerd · VMM · 内核            │
 └────────────────────┬─────────────────────────────┘
                      │ Unix socket（无认证）
 ┌────────────────────┴─────────────────────────────┐
 │ API 调用方（SDK / CLI）—— 能连上 socket 即等价 root │
 └────────────────────┬─────────────────────────────┘
                      │ vsock + VMM 硬件虚拟化隔离
 ┌────────────────────┴─────────────────────────────┐
 │ Sandbox Guest（不可信）：Agent 负载、用户代码       │
 └──────────────────────────────────────────────────┘
```

可信计算基（TCB）指必须假定其正确、否则整个安全模型不成立的那部分组件。这是被迫接受的信任前提，不代表这些组件本身比别处更安全：其中任何一项被攻破，本文描述的安全属性会同时失效，且外层防御无法兜底，因为那些防御本身就运行在 TCB 之上。TCB 应当尽可能小，上图列出的每一项都是负担而非资产。

宿主机侧的 root 上下文（含 PATH、配置文件、插件目录）全部位于信任边界之内，唯一被当作不可信输入源的是 Sandbox Guest 内部。

### conchd 以 root 运行

conchd 的核心功能依赖 root 特权：创建网络命名空间、配置 iptables、调用 CNI 插件、拉起虚拟机、挂载卷。降权运行（普通用户加有限 capability）不受支持，不应通过 systemd 的 `User=` 修改。

conchd 与它依赖的二进制、配置文件和插件目录共同构成宿主机的可信计算基。能写入这些路径的主体即等价于拥有 root 权限，这类场景不构成权限提升，属于部署配置问题。代码层面的运行时校验无法兜底，因为校验本身运行在同一 root 上下文中，可被同样的能力绕过。相关路径的加固要求见下一节。

### Sandbox 内部不可信

Sandbox 内可以执行任意命令，这是 Conch 特性。Guest 与宿主之间的边界由 VMM 和 KVM 的硬件虚拟化提供。Sandbox 内运行的代码应始终按不可信处理。

### API 无认证机制

conchd 的 HTTP API 没有认证与授权，访问控制完全依赖传输层。API 只监听本地 Unix socket（默认 `/var/run/conch/conchd.sock`），不存在 TCP 监听。能连接该 socket 即可创建 Sandbox、挂载宿主目录、操作网络，等价于 root 权限。默认部署下仅 root 可连接。

## 部署加固

以下为部署的前提条件。前提不成立时，Conch 不承诺本文描述的安全属性。

### systemd 运行环境

仓库提供的 `scripts/conchd.service` 配合 systemd 的默认行为已经满足以下前提，**无需额外的 drop-in**：

- 服务不继承调用者的环境。systemd 不会把 manager 的环境变量传给服务，PATH 取 systemd 的内置默认值（`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin`），而不是登录 shell 的 PATH。
- 工作目录默认为 `/`，这是 systemd 系统服务的默认值。
- `ExecStart` 显式传入 `--config ${CONCHD_CONFIG}`，不会触发相对路径的配置搜索（见下文「配置文件路径」）。

需要守住的是 PATH 的**内容**：其中不应出现 `.`、相对路径、`/tmp`、用户家目录或任何非 root 可写的目录，且每个目录及其每一级父目录都必须不可被非 root 写入。

> **注意：** 单元中的 `EnvironmentFile=-/etc/sysconfig/conchd` 和 `-/etc/default/conchd` 可以覆盖服务的任何环境变量，包括 PATH 和 `CONCHD_CONFIG`。systemd 中 `EnvironmentFile=` 的优先级**高于** `Environment=`，且与两者在单元里的先后顺序无关——因此只写 `Environment=PATH=...` 的 drop-in 不会生效。确实需要覆盖 PATH 时，必须先用空值重置文件列表：
>
> ```ini
> [Service]
> EnvironmentFile=
> Environment=CONCHD_CONFIG=/etc/conch/config.yaml
> Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin
> ```
>
> 重置会一并丢弃这两个文件里原有的其他变量（例如镜像拉取用的 proxy 设置），需要的变量要一起搬进 drop-in。

> **注意：** core dump 不在本节的前提条件内。转储文件为 root 属主、非 root 不可读，不构成权限提升；相关的数据扩散风险见「注意事项 - 崩溃转储」。

### 目录与文件权限

| 路径 | 用途 | 要求 |
| --- | --- | --- |
| `/etc/conch/config.yaml` | 配置文件 | `root:root 0640` |
| `/etc/conch/` | 配置目录 | `root:root 0755` |
| `/etc/sysconfig/conchd`、`/etc/default/conchd` | systemd 环境覆盖（若创建） | `root:root`，不可被非 root 写 |
| `/etc/conch/cni/net.d/` | CNI 网络配置 | `root:root 0755` |
| `/usr/libexec/cni/` | CNI 插件二进制 | `root:root 0755` |
| `/var/run/conch/` | socket、PID 文件、运行时状态 | `root:root 0750` |
| `/var/lib/conch/` | 状态库、Guest kernel/initrd、镜像与快照 | `root:root 0755`，文件不可被非 root 写 |

conchd 启动时会校验配置文件权限：组写、组执行以及 other 的任何权限都会导致启动失败，因此 `0644`、`0660` 等模式无法使用。RPM 默认发布 `0640`。

上表的依据：

- CNI 插件目录的写权限等价于守护进程的代码执行权限，conchd 会以 root 执行该目录下的二进制。
- PATH 与插件目录路径上的每一级父目录同样不可被非 root 写入，只检查末级目录不足以成立。
- 非 root 可写的运行时目录可被用于替换 kernel、initrd 或快照文件，从而在 root 上下文中执行外部控制的内容。

`/var/run/conch/` 由 conchd 在启动时以 `0750` 创建，无需额外配置。

`0750` 的意义在于收敛读取面，它不影响 API 的访问控制：socket 自身为 `0660`，`connect()` 检查的是 socket inode 的写权限，非 root 在任何目录权限下都无法连接。真正的作用有两处：

- 该目录下的 `sandboxes/<id>/volume/N` 是宿主目录的 bind 挂载点，以 `0755` 创建。bind 挂载给同一份数据开出第二条路径，而新路径的祖先链与原路径无关，源路径上限制性的父目录不再参与判定——本地非 root 用户可以经由这条路径读到原本因父目录不可进入而读不到的共享内容。目录本身的 `x` 位控制能否穿过它，因此把 `/var/run/conch/` 设为 `0750` 可在最外层一次性截断整棵子树，无需逐个收紧子目录。
- `0755` 时本地任意用户可枚举该目录，得到 Sandbox ID、数量和启动时间。

> **注意：** `os.MkdirAll` 对已存在的目录不改权限。从 conchd 以 `0755` 创建该目录的旧版本升级时，或 `server.work_dir` 指向持久化路径（不随重启清空）时，遗留目录会保持 `0755`，需要手工 `chmod 0750` 一次。`/var/run` 位于 tmpfs，重启后由 conchd 重新以 `0750` 创建，不受此影响。

如需授权特定账号调用 API，将该目录改为 `root:<管理组> 0750` 并把账号加入该组——`0750` 保留了属组的 `r-x`，收紧的只是 other；socket 自身的权限位不应放宽。

### 配置文件路径

生产环境应显式指定配置文件：

```bash
conchd --config /etc/conch/config.yaml
```

省略 `--config` 时，conchd 先查找 `/etc/conch/config.yaml`，再查找相对当前工作目录的 `config/config.yaml`。从共享目录或 `/tmp` 启动时，后者可能命中伪造配置。

配置中的 `server.work_dir`、`server.state_dir`、VMM 与 virtiofsd 二进制路径、containerd socket 路径均应指向 root 独占的目录。virtiofsd 建议写为绝对路径，不依赖 PATH 解析。

### 依赖组件

containerd 的 socket 本身即 root 等价接口，权限应保持 `root:root`。VMM（StratoVirt / Cloud Hypervisor）、virtiofsd、erofs-utils 和 iptables 建议使用发行版签名包安装，并随发行版更新。

## 注意事项

### Sandbox 之间默认可达

Conch 支持为每个 Sandbox 配置 IP 级网络策略，但默认不启用任何限制。不传 `network` 配置时，Sandbox 之间在 IP 层可达，Sandbox 到宿主的流量也不受限制。

每个 Sandbox 内的 Agent API 监听在所有网卡上，由创建时下发的访问凭据保护。该凭据是 Sandbox 之间唯一的认证边界，不应在业务侧记录或转发。

要求 Sandbox 互不可达时，需在创建时显式传入网络策略：

```python
sandbox = Sandbox(
    template_id="sha256:...",
    network={"allowOut": ["10.0.0.0/8"], "allow_internet_access": False},
)
```

支持 `allowOut`、`denyOut`、`allowIn`、`denyIn` 和 `allow_internet_access`，单个 Sandbox 最多 1024 条目标地址。字段说明见 [Python SDK](python-sdk.md)。

> **注意：** 宿主侧 `net.bridge.bridge-nf-call-iptables` 等内核开关不影响上述策略。策略下发在 Sandbox 自己的网络命名空间内，与宿主命名空间的网桥 netfilter 开关无关。

### Sandbox 访问凭据

创建 Sandbox 时 conchd 生成一个随机凭据并在响应中返回，其性质如下：

- 仅对应该 Sandbox，不能访问其他 Sandbox。
- 不写入状态库，不写入日志，conchd 重启或 Sandbox 删除后无法恢复。
- conchd 仅在创建过程中持有，不做长期保存。

凭据丢失后无法找回，只能删除并重建 Sandbox。

### Webhook 产生出站请求

注册[生命周期 Webhook](webhooks.md) 后，conchd 以 root 身份向注册的 URL 发起出站 HTTP/HTTPS 请求。

URL 校验只要求协议为 `http` 或 `https` 且主机名非空，不限制目标地址：内网地址、`127.0.0.1`、云厂商元数据端点均可注册，明文 `http://` 也会被接受。事件载荷不包含 Sandbox 访问凭据。建议优先使用 HTTPS 端点，必要时在宿主侧限制 conchd 的出站范围。

### 崩溃转储

conchd 默认不限制 core dump（`scripts/conchd.service` 中 `LimitCORE=infinity`），该限制会被 VMM 子进程继承。

崩溃转储会把两类内容写入磁盘：conchd 崩溃时其内存中在途的 Sandbox 访问凭据，以及 VMM 崩溃时该 Sandbox 的整个 Guest 内存（默认 2048 MB）。

这不构成权限提升。转储文件为 root 属主、非 root 不可读，能读到它的主体已经可以直接调用无鉴权 API 在任意 Guest 内执行命令，无需经由转储文件。同理，`LimitCORE=0` 也拦不住 `gcore`：gdb 附加后自行写文件，不受 `RLIMIT_CORE` 约束。

真正的风险是数据扩散：转储文件会长期留在磁盘上，可能进入备份、被 `coredumpctl` 导出，或在配置了崩溃上报的环境中被传出本机。Guest 内存中的业务数据随之扩散，性质与「Checkpoint 快照」一节相同——转储本质上就是一份无人管理的内存快照。

Guest 内存中承载敏感数据的部署建议通过 drop-in 关闭：

```ini
[Service]
LimitCORE=0
```

代价是失去 conchd 与 VMM 的崩溃现场。

### 共享目录

通过 virtiofs 挂载给 Sandbox 的宿主目录在 Guest 内可读写。应只暴露确实需要共享的最小路径，不共享 `/`、`/etc`、`/var/lib/conch` 等敏感路径。

### Checkpoint 快照

对 Sandbox 做 checkpoint 会把 Guest 内存写入快照，并可作为 Template 推送到镜像仓库。快照中包含 checkpoint 时刻的 Sandbox 访问凭据，但 restore 时会重新生成并下发新凭据，快照中的为失效凭据。

含业务敏感数据的 Sandbox 内存快照仍不建议推送到非受信仓库。

### 镜像来源

`conch image pull` 和 `conch template pull` 拉取的内容会进入 Guest 运行，应只使用可信仓库。

OCI 镜像用 `conch image pull` 拉取，Boot Index 必须用 `conch template pull`。

