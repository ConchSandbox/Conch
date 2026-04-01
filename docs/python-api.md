# Conch Python SDK API

## 快速开始

```python
from conch import Sandbox

try:
    sbx = Sandbox.create()
    result = sbx.execute(cmd='python3', content='print("Hello")')
    print(result)
finally:
    sbx.delete()
```

**说明：**
- `Sandbox.create()` - 创建沙箱
- `execute()` - 执行命令
- `delete()` - 清理资源（保证在 finally 块中执行）

---

## 上下文管理器

Sandbox 支持 Python 上下文管理器协议，提供更简洁的资源管理方式。

```python
from conch import Sandbox

with Sandbox.create() as sbx:
    result = sbx.execute(cmd='python3', content='print("Hello")')
    print(result)
# 自动调用 delete()
```

**优势：**
- 代码更简洁
- 自动资源管理
- 即使异常也保证清理
- 符合 Python 最佳实践

---

## Sandbox 核心方法

### 创建沙箱

```python
Sandbox.create(snapshot_id=None, **kwargs) -> Sandbox
```

基于镜像或快照创建沙箱。

**参数：**
- `snapshot_id` (可选): 从指定快照创建

**返回：** 成功返回 `Sandbox` 对象，失败抛出 `RuntimeError`

**示例：**
```python
# 从镜像创建
sbx = Sandbox.create()
sbx.execute(cmd='python3', content='print("Hello")')
sbx.delete()

# 从快照创建
sbx = Sandbox.create(snapshot_id="snap_123")
sbx.execute(cmd='python3', content='print("Restored")')
sbx.delete()

# 使用上下文管理器
with Sandbox.create() as sbx:
    sbx.execute(cmd='python3', content='print("Hello")')
```

---

### 执行命令

```python
sandbox.execute(cmd, content=None, **kwargs) -> Execution
```

在沙箱中执行命令或脚本。

**参数：**
- `cmd`: 命令（如 `python3`、`sh`）
- `content`: 要执行的脚本内容
- `cwd`: 执行目录（默认工作目录）
- `env`: 环境变量字典
- `timeout`: 超时时间（秒）

**返回：** `Execution` 对象

**示例：**
```python
result = sbx.execute(cmd='python3', content='print("Hello")')
print(result.stdout)  # 标准输出
print(result.stderr)  # 标准错误
print(result.exit_code)  # 退出码
```

---

### 暂停沙箱

```python
sandbox.pause() -> SnapshotInfo
```

暂停沙箱并创建快照，用于后续快速恢复。**注意：pause 后原沙箱会被自动清理，无需再调用 delete()**。

**返回：** `SnapshotInfo` 对象（包含 snapshot_id 和 sandbox_id）

**完整示例：快照生命周期**

```python
# 步骤 1: 从镜像创建沙箱
sbx = Sandbox.create()
print(f"Created sandbox: {sbx.sandbox_id}")

# 步骤 2: 暂停并创建快照（沙箱自动清理）
snapshot = sbx.pause()
print(f"Snapshot ID: {snapshot.snapshot_id}")

# 步骤 3: 从快照恢复，创建新沙箱
sbx2 = Sandbox.create(snapshot.snapshot_id)
print(f"Restored sandbox: {sbx2.sandbox_id}")
sbx2.delete()
```

**说明：**
- 快照保存沙箱的完整状态
- pause() 后原沙箱会被服务端自动清理，无需调用 delete()
- 从快照创建沙箱比从镜像启动更快（秒级启动）
- 每个快照都有唯一的 `snapshot_id`

---

### 删除沙箱

```python
sandbox.delete(sandbox_id=None)
```

删除沙箱实例并释放资源。

**参数：**
- `sandbox_id` (可选): 删除指定的沙箱（默认删除当前实例）

**注意：** 失败抛出 `RuntimeError`

**静态方法：**
```python
Sandbox.delete_sandbox(sandbox_id)
```

无需创建实例即可删除指定沙箱。

**示例：**
```python
# 删除当前实例
with Sandbox.create() as sbx:
    pass
# 自动删除（上下文管理器）

# 手动删除
sbx = Sandbox.create()
sbx.delete()

# 直接删除指定沙箱
Sandbox.delete_sandbox("sandbox_abc")
```

---

## Sandbox 其他方法

| 方法 | 说明 | 返回值 |
|------|------|--------|
| `get_info()` | 获取沙箱信息 | `SandboxInfo` |
| `health_check()` | 健康检查 | `dict` |
| `upload(local, remote)` | 上传文件 | `dict` |
| `download(remote, local)` | 下载文件 | `dict` |
| `list_files()` | 列出工作目录文件 | `list[str]` |

**注意：** 所有方法失败时都会抛出 `RuntimeError` 异常

---

## Sandbox 构造函数

```python
Sandbox(unix_socket=None, api_url=None, sandbox_id=None, image_name=None,
         namespace=None, snapshot_id=None, vcpu_num=None,
         ram_mb=None, workdir=None, config_path=None)
```

**主要参数：**
- `unix_socket`: Unix socket 路径（默认从配置读取）
- `api_url`: 服务地址；仅当 `unix_socket` 为空时使用
- `sandbox_id`: 沙箱 ID（默认自动生成）
- `image_name`: 镜像名称
- `snapshot_id`: 快照 ID

**注意：** 构造函数仅初始化本地状态，不创建沙箱。请使用 `Sandbox.create()` 类方法。

---

## 数据类型

### SandboxInfo

```python
@dataclass
class SandboxInfo:
    sandbox_id: str
    ip: str
    snapshot_id: Optional[str]
    vcpu_num: int
    ram_mb: int
```

### SnapshotInfo

```python
@dataclass
class SnapshotInfo:
    snapshot_id: str
    sandbox_id: str
```

### Execution

```python
class Execution:
    stdout: str      # 标准输出
    stderr: str      # 标准错误
    exit_code: int   # 退出码
    logs: str       # 合并输出
```

---

## AgentClient（低级 API）

沙箱内代理客户端，由 Sandbox 内部管理。通常不需要直接使用。

| 方法 | 说明 |
|------|------|
| `health_check()` | 健康检查 |
| `start_process()` | 启动进程 |
| `post_files()` | 上传文件 |
| `get_file()` | 下载文件 |
| `close()` | 关闭连接 |

**注意：** 如需直接使用，通过 `sandbox.client` 属性访问。

---

## 完整示例

### 示例 1: 基本使用（try-finally）

```python
from conch import Sandbox

try:
    sbx = Sandbox.create()
    info = sbx.get_info()
    print(f"Created sandbox: {info.sandbox_id}, IP: {info.ip}")

    # 执行命令
    result = sbx.execute(cmd='python3', content='print("Hello!")')
    print(result.stdout)

    # 上传文件
    sbx.upload('local.txt', 'remote.txt')

    # 下载文件
    sbx.download('remote.txt', 'downloaded.txt')

    # 列出文件
    files = sbx.list_files()
    print(f"Files: {files}")
finally:
    sbx.delete()
```

### 示例 2: 基本使用（上下文管理器）

```python
from conch import Sandbox

with Sandbox.create() as sbx:
    info = sbx.get_info()
    print(f"Created sandbox: {info.sandbox_id}, IP: {info.ip}")

    # 执行命令
    result = sbx.execute(cmd='python3', content='print("Hello!")')
    print(result.stdout)

    # 上传文件
    sbx.upload('local.txt', 'remote.txt')

    # 下载文件
    sbx.download('remote.txt', 'downloaded.txt')

    # 列出文件
    files = sbx.list_files()
    print(f"Files: {files}")
```

### 示例 3: 快照功能

```python
from conch import Sandbox

# 创建并暂停
sbx = Sandbox.create()
snapshot = sbx.pause()
print(f"Created snapshot: {snapshot.snapshot_id}")

# 从快照恢复
sbx2 = Sandbox.create(snapshot.snapshot_id)
sbx2.execute(cmd='python3', content='print("Restored!")')
sbx2.delete()
```

### 示例 4: 异常处理

```python
from conch import Sandbox

try:
    sbx = Sandbox.create()
    result = sbx.execute(cmd='invalid_command')
except RuntimeError as e:
    print(f"Error: {e}")
finally:
    sbx.delete()
```
