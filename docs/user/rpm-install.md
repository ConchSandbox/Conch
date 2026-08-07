# RPM 安装与服务管理

## 安装

```bash
sudo dnf install conch
sudo systemctl enable --now conchd
```

服务名为 `conchd`，配置文件为 `/etc/conch/config.yaml`，日志写入 journald：

```bash
systemctl status conchd
journalctl -u conchd -f
```

## 服务管理

```bash
sudo systemctl start conchd
sudo systemctl stop conchd
sudo systemctl restart conchd
```

> **注意：** conchd 重启后不会接管已有的 Sandbox 和 VMM 进程。停止或重启服务前，请先删除所有活跃 Sandbox。

如需覆盖配置文件或追加启动参数，在 `/etc/sysconfig/conchd` 中设置：

```bash
CONCHD_CONFIG=/etc/conch/config-dev.yaml
CONCHD_OPTS=
```

修改后重启 conchd。需要修改 systemd 单元时，使用 `systemctl edit conchd` 创建 drop-in。

## 卸载

```bash
sudo dnf remove conch
```
