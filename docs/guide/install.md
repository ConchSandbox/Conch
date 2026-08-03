# Conch 安装与服务管理

Conch 支持两种部署方式：RPM 包安装（conchd 由 systemd 管理）和源码编译（前台运行二进制）。

## 1. RPM 安装

```bash
dnf install conch
```

安装后 conchd 已注册为 systemd 服务，但不会自动启动。

包内与运行相关的文件：

| 路径 | 说明 |
|------|------|
| `/usr/bin/conchd`、`/usr/bin/conch` | 守护进程和 CLI |
| `/usr/bin/conch-init` | guest 内的 init，同时打包进 initrd |
| `/etc/conch/config.yaml` | conchd 配置，`%config(noreplace)`，升级不覆盖本地修改 |
| `/etc/conch/cni/net.d/10-conch.conf` | 默认 CNI 网络配置 |
| `/var/lib/conch/kernel` | guest 内核 |
| `/var/lib/conch/conch.initrd` | guest initrd |
| `/usr/lib/systemd/system/conchd.service` | systemd 单元 |

CNI 插件由 `containernetworking-plugins` 依赖带入，安装在 `/usr/libexec/cni`，与 `config.yaml` 中 `network.cni.plugin_bin_dirs` 的默认值一致。

## 2. 服务管理

```bash
systemctl start conchd
systemctl stop conchd
systemctl restart conchd
systemctl status conchd
systemctl enable conchd       # 开机自启
journalctl -u conchd -f       # 跟踪日志
```

服务名为 `conchd`，`conch` 是 CLI 的名字，不是服务名。

### 服务行为

- **`Type=notify`**：conchd 在监听套接字就绪后通过 sd_notify 上报 `READY=1`，`systemctl start` 会阻塞到这一刻才返回。因此启动后立即执行 `conch image pull`、`conch template ls` 不会出现连接失败。
- **`KillMode=process`**：停止或重启时 systemd 只终止 conchd 本身，沙箱 VMM 进程继续运行，conchd 下次启动时按 PID 重新接管，网络 slot 和 state.db 记录一并保留。
- **优雅退出**：conchd 收到 `SIGTERM` 后上报 `STOPPING=1`，依次关闭 HTTP server（在途请求最多等 30s）、删除套接字、关闭 containerd 和 state store。`TimeoutStopSec=60` 覆盖这一预算。
- **`Restart=on-failure` / `RestartSec=2`**：异常退出后自动重启；`systemctl stop` 主动停止时不重启。连续失败 5 次（`StartLimitBurst=5` / `StartLimitIntervalSec=60`）后置为 `failed`，需 `systemctl reset-failed conchd` 才能再次启动。
- **`ExecStartPre` 加载内核模块**：启动前尝试 `modprobe erofs`（containerd 的 erofs snapshotter 依赖它，未加载时 conchd 直接启动失败）和 `modprobe vhost_vsock`（stratovirt 的 `vhost-vsock-pci` 依赖它，缺失时服务能起来但创建沙箱会失败）。两条都带 `-` 前缀，内核已内置这些模块或宿主机没有 `/lib/modules` 时不会因此判定启动失败。
- **日志**：配置默认 `log.output: stdout`，日志进入 journald。

### 配置覆盖

不要直接改单元文件（升级会被覆盖），改用环境文件 `/etc/sysconfig/conchd`：

```bash
# 使用非默认配置文件
CONCHD_CONFIG=/etc/conch/config-dev.yaml

# 追加命令行参数
CONCHD_OPTS=
```

改完执行 `systemctl restart conchd` 生效。需要调整单元本身（如资源限制）时用 `systemctl edit conchd` 建 drop-in，升级不会覆盖。

## 3. 源码编译运行

```bash
./scripts/conch-env-setup.sh install   # 安装依赖、编译、装 Python SDK
./bin/conchd                           # 前台运行
```

前台运行时日志直接输出到终端，适合开发调试。`NOTIFY_SOCKET` 环境变量不存在，sd_notify 上报自动跳过，不影响运行。

### 与 systemd 服务共存

两种方式可以共存，但**同一时刻只能有一个 conchd 在跑**，由 pid 文件保证：

- 服务已运行时执行 `./bin/conchd`：立即失败退出，`Failed to acquire pid file error=pid file /var/run/conchd/conchd.pid already exists and process N is still running`。失败发生在初始化任何子系统之前，运行中的服务不受影响。
- 手动实例运行时执行 `systemctl start conchd`：服务因 pid 文件启动失败，重试 5 次后置为 `failed`，手动实例不受影响。停掉手动实例后 `systemctl reset-failed conchd && systemctl start conchd` 即可。

两点容易踩的差异：

1. **配置文件优先级**：`FindConfigFile` 先找 `/etc/conch/config.yaml`，再找 `./config/config.yaml`。装过 RPM 后前者已存在，在仓库根目录直接跑 `./bin/conchd` 用的也是它，改仓库里的 `config/config.yaml` 不会生效。要用仓库配置须显式指定：`./bin/conchd --config config/config.yaml`。
2. **二进制版本**：服务跑的是 `/usr/bin/conchd`，重新编译源码只更新 `./bin/conchd`。验证源码改动请直接运行 `./bin/conchd`。

## 4. 卸载

```bash
dnf remove conch
```

`%systemd_preun` 会在卸载前停止并禁用服务。`/etc/conch` 下被修改过的配置文件以 `.rpmsave` 保留。

## 5. 排障

先看日志：`journalctl -u conchd -n 100 --no-pager`。

| 现象 | 原因与处理 |
|------|-----------|
| `Unit conchd.service not found` | 单元缺失，检查 `/usr/lib/systemd/system/conchd.service`，必要时重装包 |
| `EROFS unsupported, please modprobe erofs` | 单元的 `ExecStartPre` 已尝试 `modprobe erofs`，仍报此错说明内核没有该模块，需换用带 erofs 的内核 |
| 服务正常但创建沙箱失败、无 `/dev/vhost-vsock` | `vhost_vsock` 模块未能加载，确认内核提供该模块 |
| `pid file ... already exists and process N is still running` | 已有 conchd 在服务外运行，先 `kill N` 再启动服务 |
| 服务反复失败后变为 `failed` | 排除故障后需 `systemctl reset-failed conchd` |
| 启动超时失败 | `TimeoutStartSec=300`，检查日志中最后一个初始化阶段 |
| 找不到 CNI 插件 | 确认 `containernetworking-plugins` 已安装、`/usr/libexec/cni` 下有 `bridge`、`host-local`、`loopback` |
| 改了单元文件未生效 | 执行 `systemctl daemon-reload` |
| 装了新包仍在跑旧配置 | `/etc/systemd/system` 优先级高于 `/usr/lib/systemd/system`，检查前者下是否有遗留的 `conchd.service` |
