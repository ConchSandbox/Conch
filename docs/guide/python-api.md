# Conch Python SDK API

## 快速开始

在终端中输入 `python3` 进入交互环境，逐行输入以下命令：

```
>>> from conch import Sandbox
>>> sandbox = Sandbox.create()
>>> sandbox.sandbox_id
'sandbox_a1b2c3d4e5f6'
>>> result = sandbox.execute(cmd="ls", args=["-l", "/root"])
>>> print(result)
total 0
drwxr-xr-x 2 root root 4096 Apr 22 10:00 .
drwxr-xr-x 2 root root 4096 Apr 22 10:00 ..
>>> sandbox.delete()
True
```

**说明：**
- `Sandbox.create()` - 创建沙箱
- `execute()` - 执行命令
- `delete()` - 清理资源

生产代码建议使用 `with Sandbox.create() as sandbox:` 上下文管理器，自动调用 `delete()`。

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

基于镜像或快照创建沙箱。`**kwargs` 可透传所有构造函数参数（如 `image_name`、`vcpu_num`、`ram_mb`、`namespace`、`config_path` 等）。

**参数：**
- `snapshot_id` (可选): 从指定快照创建
- `**kwargs`: 透传至构造函数，参见 [Sandbox 构造函数](#sandbox-构造函数)

**返回：** 成功返回 `Sandbox` 对象，失败抛出 `RuntimeError`

**Direct API：** `POST /api/v1/sandboxes`

**示例：**
```python
# 从config设置镜像创建
sbx = Sandbox.create()
sbx.execute(cmd='python3', content='print("Hello")')
sbx.delete()

# 从传入指定镜像创建
sbx = Sandbox.create(image_name="hub.oepkgs.net/conch/conch-base:v0.1")
sbx.execute(cmd='python3', content='print("Hello")')
sbx.delete()

# 从快照创建
sbx = Sandbox.create(snapshot_id="snap_123")
sbx.execute(cmd='python3', content='print("Restored")')
sbx.delete()

# 从快照镜像创建
sbx = Sandbox.create(
    image_name="hub.oepkgs.net/conch/conch-snapshot:v0.1",
    use_snapshot=True,
)
sbx.execute(cmd='python3', content='print("Restored from snapshot image")')
sbx.delete()

# 指定自定义配置文件
sbx = Sandbox.create(config_path="/path/to/sdk-config.yaml")

# 使用上下文管理器
with Sandbox.create() as sbx:
    sbx.execute(cmd='python3', content='print("Hello")')
```

---

### 查询沙箱列表

```python
Sandbox.list(unix_socket=None, api_url=None, namespace=None,
             config_path=None) -> list[dict]
```

查询指定 namespace 下的 sandbox 摘要列表。该接口主要用于管理端展示已有沙箱状态；后续如需对某个沙箱执行操作，可通过 `Sandbox.get()` 恢复为 sandbox 引用。

**参数：**
- `unix_socket` (str, 可选): conchd Unix socket 路径
- `api_url` (str, 可选): conchd HTTP 服务地址
- `namespace` (str, 可选): 命名空间，不指定时使用服务端默认 namespace
- `config_path` (str, 可选): SDK 配置文件路径

**返回：** `list[dict]`，每个元素为一个 sandbox 摘要信息。

**Direct API：** `GET /api/v1/sandboxes`

**示例：**
```python
sandboxes = Sandbox.list()
for item in sandboxes:
    print(item["sandbox_id"], item.get("state"), item.get("ip"))
```

返回值示例：

```python
[
    {
        "sandbox_id": "sandbox_a1b2c3d4e5f6",
        "pod_sandbox_id": "sandbox_a1b2c3d4e5f6",
        "namespace": "default",
        "state": "READY",
        "image_name": "hub.oepkgs.net/conch/conch-base:v0.1",
        "ip": "172.18.0.2",
        "vcpu_num": 2,
        "ram_mb": 2048,
        "created_at": 1713760800000000000
    }
]
```

---

### 获取指定沙箱

```python
Sandbox.get(sandbox_id, unix_socket=None, api_url=None,
            namespace=None, config_path=None) -> Sandbox
```

根据 `sandbox_id` 查询指定 sandbox 的摘要信息，并恢复为一个 `Sandbox` 对象。该对象可继续用于 `delete()`、`logs()`、`update_network()` 等实例级控制面操作。

**参数：**
- `sandbox_id` (str): 沙箱 ID
- `unix_socket` (str, 可选): conchd Unix socket 路径
- `api_url` (str, 可选): conchd HTTP 服务地址
- `namespace` (str, 可选): 命名空间
- `config_path` (str, 可选): SDK 配置文件路径

**返回：** 成功返回 `Sandbox` 对象，失败抛出 `RuntimeError`

**Direct API：** `GET /api/v1/sandboxes/{sandbox_id}`

**示例：**
```python
sbx = Sandbox.get("sandbox_a1b2c3d4e5f6")
info = sbx.get_info()
print(info.sandbox_id, info.ip)
print(sbx.logs(limit=20))
```

---

### 执行命令

```python
sandbox.execute(cmd, content=None, args=[], cwd=None, env={}) -> Execution
```

在沙箱中执行命令或脚本。

**参数：**
- `cmd` (str): 命令名称（如 `python3`、`ls`、`sh`）
- `content` (str, 可选): 脚本内容，传入时自动写入临时文件并执行
- `args` (list, 可选): 命令参数列表
- `cwd` (str, 可选): 执行目录，不指定时使用用户家目录
- `env` (dict, 可选): 环境变量，会追加到沙箱默认环境变量中

**返回：** `Execution` 对象

**示例：**
```python
# 执行 Python 脚本
result = sbx.execute(cmd='python3', content='print("Hello")')
print(result.stdout)
print(result.exit_code)

# 执行带参数的系统命令
result = sbx.execute(cmd='ls', args=['-l', '/root'])
print(result)

# 指定工作目录
result = sbx.execute(cmd='python3', content='import os; print(os.getcwd())', cwd='/tmp')

# 指定环境变量（需要通过 shell 展开，参见 FAQ）
result = sbx.execute(cmd='sh', args=['-c', 'echo $MY_VAR'],
                     env={'MY_VAR': 'conch_test'})
```

---

### 暂停沙箱

```python
sandbox.pause() -> SnapshotInfo
```

暂停沙箱并创建快照，用于后续快速恢复。**注意：pause 后原沙箱会被自动清理，无需再调用 delete()**。

**返回：** `SnapshotInfo` 对象（包含 `snapshot_id` 和 `sandbox_id`）

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
sandbox.delete(sandbox_id=None) -> bool
```

删除沙箱实例并释放资源。

**参数：**
- `sandbox_id` (可选): 删除指定的沙箱（默认删除当前实例）

**返回：** 成功返回 `True`，失败抛出 `RuntimeError`

**Direct API：** `DELETE /api/v1/sandboxes/{sandbox_id}`

**静态方法：**
```python
Sandbox.delete_sandbox(sandbox_id, unix_socket=None, api_url=None,
                       namespace=None, config_path=None) -> bool
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

### 更新沙箱网络策略

```python
sandbox.update_network(allow_out=None, deny_out=None,
                       egress_proxy=None,
                       allow_public_traffic=None) -> dict
```

更新指定 sandbox 的网络策略。该接口字段参考 E2B network 配置形式，但 Conch 不使用 E2B traffic/domain token 语义，具体执行语义以后端 netstack / policy 实现为准。

**参数：**
- `allow_out` (bool, 可选): 是否允许出站访问
- `deny_out` (list[str], 可选): 出站拒绝规则列表
- `egress_proxy` (str, 可选): 出站代理地址
- `allow_public_traffic` (bool, 可选): 是否允许公网入站访问

**返回：** `dict`，包含更新结果和网络策略摘要。

**Direct API：** `PUT /api/v1/sandboxes/{sandbox_id}/network`

**示例：**
```python
sbx = Sandbox.create()

resp = sbx.update_network(
    allow_out=True,
    deny_out=["example.com", "10.0.0.0/8"],
    egress_proxy="http://127.0.0.1:7890",
    allow_public_traffic=False,
)

print(resp)
sbx.delete()
```

返回值示例：

```python
{
    "status": "ok",
    "sandbox_id": "sandbox_a1b2c3d4e5f6",
    "network": {
        "sandbox_id": "sandbox_a1b2c3d4e5f6",
        "allowOut": True,
        "denyOut": ["example.com", "10.0.0.0/8"],
        "egressProxy": "http://127.0.0.1:7890",
        "allowPublicTraffic": False
    }
}
```

---

### 查询沙箱审计日志

```python
sandbox.logs(cursor=None, limit=None, level=None, search=None) -> dict
```

查询指定 sandbox 的控制面审计日志。该日志用于记录 sandbox 生命周期和管理操作，例如 create、pause、delete、update_network 等。

**注意：** 该接口不等同于命令执行日志，不返回 `execute()` 的 stdout/stderr；命令输出请从 `Execution.stdout`、`Execution.stderr` 或 `Execution.logs` 获取。

**参数：**
- `cursor` (str, 可选): 分页游标
- `limit` (int, 可选): 返回条数限制
- `level` (str, 可选): 日志级别过滤，例如 `info`、`warn`、`error`
- `search` (str, 可选): 按操作名或消息内容搜索

**返回：** `dict`，包含 `logs`、`next_cursor`、`has_more` 字段。

**Direct API：** `GET /api/v1/sandboxes/{sandbox_id}/logs`

**示例：**
```python
sbx = Sandbox.create()

logs = sbx.logs(limit=20)
for item in logs["logs"]:
    print(item["timestamp"], item["level"], item["event"], item["message"])

sbx.delete()
```

返回值示例：

```python
{
    "logs": [
        {
            "id": "sandbox_a1b2c3d4e5f6-1713760800000000000-create",
            "sandbox_id": "sandbox_a1b2c3d4e5f6",
            "timestamp": 1713760800000000000,
            "level": "info",
            "event": "create",
            "message": "sandbox created"
        }
    ],
    "next_cursor": "",
    "has_more": False
}
```

---

## 文件操作

### 上传文件

```python
sandbox.upload(local_path, remote_path) -> dict
sandbox.upload([spec, ...]) -> dict
```

上传文件到沙箱。支持两种调用方式，沙箱端父目录不存在时会自动创建。

**方式 1：上传本地文件**

```python
# 上传单个本地文件到沙箱
result = sbx.upload('./local.txt', '/home/user/remote.txt')
```

**方式 2：批量上传（内存内容）**

```python
# 直接传入内容，无需本地文件
result = sbx.upload([
    {"filepath": "/home/user/a.txt", "content": b"hello"},
    {"filepath": "/home/user/b.txt", "content": b"world"},
])
```

**返回值：** `dict`，包含以下字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `status` | int | `0` 成功，`-1` 失败 |
| `uploaded_count` | int | 成功上传的文件数 |
| `message` | str | 结果描述 |

---

### 下载文件

```python
sandbox.download(remote_path, local_path) -> dict
```

从沙箱下载文件到本地。本地父目录不存在时会自动创建。

**示例：**
```python
# 从沙箱下载文件到本地
result = sbx.download('/home/user/output.txt', './downloaded.txt')
print(result)
# {'status': 0, 'size': 1024, 'message': 'OK'}
```

**返回值：** `dict`，包含以下字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `status` | int | `0` 成功，`-1` 失败 |
| `size` | int | 下载文件大小（字节） |
| `message` | str | 结果描述 |

---

### 列出文件

```python
sandbox.list_files(path=None) -> list[str]
```

列出沙箱内指定目录的所有文件。

**参数：**
- `path` (str, 可选): 目录路径，不指定时列出当前目录

**示例：**
```python
# 列出沙箱当前目录所有文件
files = sbx.list_files()
print(files)

# 列出指定目录
files = sbx.list_files('/home/user')
```

**返回值：** `list[str]`，文件路径列表。执行失败时返回空列表。

---

## 健康检查

### conchd 控制面健康检查

```python
Sandbox.service_health(unix_socket=None, api_url=None,
                       config_path=None) -> bool
```

检查 conchd 控制面服务是否可用。该接口用于判断 SDK 是否能够连接到 conchd，不依赖某个具体 sandbox 实例。

**参数：**
- `unix_socket` (str, 可选): conchd Unix socket 路径
- `api_url` (str, 可选): conchd HTTP 服务地址
- `config_path` (str, 可选): SDK 配置文件路径

**返回：** 服务可用返回 `True`，否则返回 `False`

**Direct API：** `GET /health`

**示例：**
```python
ok = Sandbox.service_health()
print(ok)

# 使用 TCP 地址显式检查
ok = Sandbox.service_health(api_url="http://127.0.0.1:4063", unix_socket="")
print(ok)
```

---

### 沙箱内 Agent 健康检查

```python
sandbox.health_check() -> dict
```

检查沙箱内 Agent 服务的健康状态。

**示例：**
```python
result = sbx.health_check()
print(result)
# {'status': 'OK', 'message': 'OK'}
```

**返回值：** `dict`，包含以下字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `status` | str | `'OK'` 正常，`'ERROR'` 异常 |
| `message` | str | 状态描述 |

---

## 获取沙箱信息

```python
sandbox.get_info() -> SandboxInfo
```

获取沙箱的基本信息。

**示例：**
```python
info = sbx.get_info()
print(f"ID: {info.sandbox_id}, IP: {info.ip}, Snapshot: {info.snapshot_id}")
```

**返回值：** `SandboxInfo` 对象，参见 [数据类型](#sandboxinfo)。

---

## Sandbox 构造函数

```python
Sandbox(unix_socket=None, api_url=None, sandbox_id=None, image_name=None,
        namespace=None, snapshot_id=None, vcpu_num=None,
        ram_mb=None, config_path=None, use_snapshot=False)
```

**主要参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `unix_socket` | str | Unix socket 路径，默认从配置文件读取 |
| `api_url` | str | 服务地址，仅当 `unix_socket` 为空时使用 |
| `sandbox_id` | str | 沙箱 ID，默认自动生成 |
| `image_name` | str | 镜像名称 |
| `namespace` | str | 命名空间 |
| `snapshot_id` | str | 快照 ID |
| `vcpu_num` | int | 虚拟 CPU 数量 |
| `ram_mb` | int | 内存大小（MB） |
| `config_path` | str | 配置文件路径，默认按优先级自动查找 |
| `use_snapshot` | bool | 是否将 `image_name` 作为快照镜像处理 |

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
```

### SnapshotInfo

```python
@dataclass
class SnapshotInfo:
    snapshot_id: str
    sandbox_id: str
```

### Sandbox 审计日志条目

`sandbox.logs()` 返回的 `logs` 字段是审计日志条目列表，每个条目通常包含：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | str | 日志 ID |
| `sandbox_id` | str | 沙箱 ID |
| `timestamp` | int | 日志时间戳 |
| `level` | str | 日志级别 |
| `event` | str | 操作事件，例如 `create`、`pause`、`delete`、`update_network` |
| `message` | str | 日志描述 |

### Execution

```python
class Execution:
    stdout: str      # 标准输出
    stderr: str      # 标准错误
    exit_code: int   # 退出码
    logs: str        # 合并输出（stdout + stderr）
```

`str(execution)` 返回合并输出（`logs.strip()`）。

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

sbx = None
try:
    sbx = Sandbox.create()
    info = sbx.get_info()
    print(f"Created sandbox: {info.sandbox_id}, IP: {info.ip}")

    # 执行命令
    result = sbx.execute(cmd='python3', content='print("Hello!")')
    print(result.stdout)

    # 上传文件
    sbx.upload('./local.txt', '/home/user/remote.txt')

    # 下载文件
    sbx.download('/home/user/remote.txt', './downloaded.txt')

    # 列出文件
    files = sbx.list_files()
    print(f"Files: {files}")
except RuntimeError as e:
    print(f"Error: {e}")
finally:
    if sbx:
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
    sbx.upload('./local.txt', '/home/user/remote.txt')

    # 下载文件
    sbx.download('/home/user/remote.txt', './downloaded.txt')

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

sbx = None
try:
    sbx = Sandbox.create()
    result = sbx.execute(cmd='invalid_command')
except RuntimeError as e:
    print(f"Error: {e}")
finally:
    if sbx:
        sbx.delete()
```

### 示例 5: 控制面管理接口

```python
from conch import Sandbox

# 检查 conchd 控制面服务
print("service health:", Sandbox.service_health())

# 查询 sandbox 摘要列表
for item in Sandbox.list():
    print(item["sandbox_id"], item.get("state"))

# 获取指定 sandbox 引用并查询审计日志
sbx = Sandbox.get("sandbox_a1b2c3d4e5f6")
print(sbx.logs(limit=10))

# 更新网络策略
sbx.update_network(
    allow_out=True,
    deny_out=["example.com"],
    allow_public_traffic=False,
)
```

---

## FAQ

### 为什么 `execute(cmd='echo', args=['$HOME'])` 输出的是 `$HOME` 而不是实际路径？

`execute()` 直接调用目标命令二进制，不经过 shell。`$HOME` 是 shell 变量语法，只有 shell 才会展开它。

**错误写法：**
```python
sbx.execute(cmd='echo', args=['$HOME'])
# 输出: $HOME（原样输出，echo 不做变量展开）
```

**正确写法：** 通过 `sh -c` 让 shell 执行：
```python
sbx.execute(cmd='sh', args=['-c', 'echo $HOME'])
# 输出: /root（shell 展开了变量）
```

同样的规则适用于管道、重定向、通配符等 shell 特性，都需要通过 `sh -c` 执行。