# Conch Python SDK API

## 快速开始

创建 Sandbox 前，需要先启动 `conchd`，并准备一个处于 `READY` 状态的 Template。可以使用 `conch template ls` 查看现有 Template；如果尚未创建，参见 [Conch Image Guide](image.md#1-conch-template-create)。

在终端中输入 `python3` 进入交互环境：

```pycon
>>> from conch import Sandbox
>>> sandbox = Sandbox.create(template_id="tmpl_xxx")
>>> sandbox.sandbox_id
'sandbox_a1b2c3d4e5f6789012345678'
>>> result = sandbox.commands.run(cmd="printf", args=["hello Conch\n"])
>>> print(result.stdout, end="")
hello Conch
>>> sandbox.delete()
True
```

**说明：**
- `Sandbox.create(template_id=...)` - 创建沙箱
- `commands.run()` - 执行命令
- `delete()` - 清理资源

也可使用 `with Sandbox.create(template_id="tmpl_xxx") as sandbox:` 上下文管理器，自动调用 `delete()`。

---

## 上下文管理器

Sandbox 支持 Python 上下文管理器协议，提供更简洁的资源管理方式。

```python
from conch import Sandbox

with Sandbox.create(template_id="tmpl_xxx") as sbx:
    result = sbx.commands.run(cmd='python3', content='print("Hello")')
    print(result)
# 自动调用 delete()
```

---

## Sandbox 核心方法

### 创建沙箱

```python
Sandbox.create(template_id, **kwargs) -> Sandbox
```

基于 Template 创建沙箱。`**kwargs` 可透传其他构造函数参数（如 `vcpu_num`、`vcpu_max`、`ram_mb`、`namespace`、`config_path` 等）。

**参数：**
- `template_id` (str): 要启动的 `tmpl_xxx`
- `**kwargs`: 透传至构造函数，参见 [Sandbox 构造函数](#sandbox-构造函数)

**返回：** 成功返回 `Sandbox` 对象。

**异常：** 配置文件不存在时抛出 `FileNotFoundError`；配置格式无效或缺少 `template_id` 时抛出 `ValueError`；缺少必需的配置段或字段时可能抛出 `KeyError`；请求 conchd 失败时抛出 `RuntimeError`。

**示例：**
```python
# 从指定 Template 创建
sbx = Sandbox.create(template_id="tmpl_123")
sbx.commands.run(cmd='python3', content='print("Hello")')
sbx.delete()

# 从 checkpoint 产生的可恢复 Template 创建
sbx = Sandbox.create(template_id="tmpl_123")
sbx.commands.run(cmd='python3', content='print("Restored")')
sbx.delete()

# 指定自定义配置文件
sbx = Sandbox.create(template_id="tmpl_123",
                     config_path="/path/to/sdk-config.yaml")

# 使用上下文管理器
with Sandbox.create(template_id="tmpl_123") as sbx:
    sbx.commands.run(cmd='python3', content='print("Hello")')
```

---

### 执行命令

```python
sandbox.commands.run(cmd, args=[], cwd=None, env={}, content=None, background=False, tag=None, pty=None, on_stdout=None, on_stderr=None) -> CommandResult | CommandHandle
```

在沙箱中执行命令或脚本。

**参数：**
- `cmd` (str): 命令名称（如 `python3`、`ls`、`sh`）
- `content` (str, 可选): 脚本内容，传入时自动写入临时文件并执行
- `args` (list, 可选): 命令参数列表
- `cwd` (str, 可选): 执行目录，不指定时使用用户家目录
- `env` (dict, 可选): 环境变量，会追加到沙箱默认环境变量中
- `background` (bool, 可选): 为 `True` 时启动后台进程并返回 `CommandHandle`
- `tag` (str, 可选): 后台进程标签，可用于后续 `connect/list/kill`
- `pty` (dict, 可选): PTY 配置，例如 `{"cols": 80, "rows": 24}`

`content` 模式由 conch-init 在沙箱内创建临时脚本并执行，不能和脚本文件参数混用。需要指定脚本文件路径时，先使用 `sandbox.files.write()` 写入文件，再通过 `args` 执行该文件。

**返回：** `background=False` 时返回 `CommandResult` 对象，`background=True` 时返回 `CommandHandle` 对象。前台命令非零退出时抛 `CommandExitException`，异常对象包含 `stdout`、`stderr`、`exit_code` 和 `error`。

前台命令可额外传入 `on_stdout`、`on_stderr` 回调；PTY 输出通过 `CommandHandle.wait(on_pty=...)` 消费。

**示例：**
```python
# 执行 Python 脚本
result = sbx.commands.run(cmd='python3', content='print("Hello")')
print(result.stdout)
print(result.exit_code)

# 执行带参数的系统命令
result = sbx.commands.run(cmd='ls', args=['-l', '/root'])
print(result)

# 指定工作目录
result = sbx.commands.run(cmd='python3', content='import os; print(os.getcwd())', cwd='/tmp')

# 指定脚本文件路径时使用文件接口
sbx.files.write('/tmp/app.py', 'print("Hello")')
result = sbx.commands.run(cmd='python3', args=['/tmp/app.py'])

# 指定环境变量（需要通过 shell 展开，参见 FAQ）
result = sbx.commands.run(cmd='sh', args=['-c', 'echo $MY_VAR'],
                     env={'MY_VAR': 'conch_test'})

# 前台流式回调
chunks = []
result = sbx.commands.run(
    cmd='sh',
    args=['-c', 'printf foo; printf bar >&2'],
    on_stdout=lambda text: chunks.append(("stdout", text)),
    on_stderr=lambda text: chunks.append(("stderr", text)),
)
```

后台命令：

```python
command = sbx.commands.run(
    cmd='python3',
    args=['-m', 'http.server', '18080'],
    cwd='/tmp',
    background=True,
    tag='http-srv',
)

# 对 http.server 这类长期服务，不要直接裸跑无限 connect 循环。
# 推荐先用普通命令验证服务可用，再按需停止后台进程。
result = sbx.commands.run(
    cmd='curl',
    args=['-sI', 'http://127.0.0.1:18080'],
)
print(result.stdout)

command.kill(signal=15)
```

后台命令和 `connect()` 返回的 `CommandHandle` 采用 E2B 风格的 `wait()` 消费输出：

```python
handle = sbx.commands.run(cmd='python3', args=['-m', 'http.server', '18080'], background=True)
result = handle.wait(
    on_pty=lambda text: print(text, end=''),
    on_stdout=lambda text: print(text, end=''),
    on_stderr=lambda text: print(text, end=''),
)

handle = sbx.commands.connect(tag='http-srv')
result = handle.wait(
    on_pty=lambda text: print(text, end=''),
    on_stdout=lambda text: print(text, end=''),
)
```

---

### 后台进程管理

```python
sandbox.commands.connect(pid=None, tag=None) -> CommandHandle
sandbox.commands.list() -> list[ProcessInfo]
sandbox.commands.kill(pid=None, tag=None, signal=15) -> bool
command.wait() -> CommandResult
command.kill(signal=15) -> bool
```

后台进程可通过 `pid` 或 `tag` 连接和发送信号。`list()` 返回当前由 conch-init 管理的后台进程列表，`signal` 必须是非 0 信号编号，默认 `15`。

**示例：**
```python
# 列出当前由 conch-init 管理的后台进程
processes = sbx.commands.list()

# 直接连接输出流，返回 CommandHandle
command = sbx.commands.connect(tag='http-srv')
print(command)  # process handle (pid=..., tag=http-srv)

try:
    for stdout, stderr, pty in command:
        print(stdout or stderr or pty or '', end='')
except KeyboardInterrupt:
    command.disconnect()

# 发送信号
sbx.commands.kill(tag='http-srv', signal=15)
```

`commands.connect()` 返回 `CommandHandle`。除兼容性的迭代方式外，更推荐用 `handle.wait()` 读取输出，和 E2B 的使用方式一致。对于 `python -m http.server`、Web 服务、worker 等长期运行进程，`handle.wait(...)` 会一直等待新输出，直到进程退出、连接断开或代码主动结束消费。在交互式 REPL 中查看长期服务输出时，应在合适时机调用 `command.disconnect()`；如果只是验证服务是否启动，推荐另起一次 `commands.run()` 执行 `curl`/业务请求，然后用 `command.kill()` 或 `commands.kill()` 停止后台进程。

直接打印 `CommandHandle` 会显示目标进程选择器，例如 `process handle (pid=42, tag=http-srv)`。目标进程不存在时，`commands.connect()` 抛出 `NotFoundError`。`commands.kill()` 成功发送信号返回 `True`，目标进程不存在时返回 `False`；请求参数错误、认证失败、网络错误或其它 conch-init 错误会映射为 SDK 错误类型后抛出。

后台命令输出来自 conch-init 的流式事件。启动后台命令后，建议通过返回的 `CommandHandle.wait()` 或 `commands.connect()` 及时消费输出；如果后台进程持续大量输出而客户端长期不读取，过量输出事件可能被丢弃，但进程本身不会因此阻塞。

启用 `pty` 时，`CommandHandle.wait()` 可以通过 `on_pty` 接收 PTY 输出；当前 SDK 兼容行为会把 PTY 输出累计到 `CommandResult.stdout`。

---

### Checkpoint Sandbox

```python
sandbox.checkpoint() -> TemplateInfo
```

捕获沙箱当前状态并返回一个可恢复 Template。Checkpoint 是作用于 Sandbox 的动作，不是独立资源；该动作不会停止或删除原沙箱。

**返回：** `TemplateInfo` 对象（包含 `template_id` 和 `sandbox_id`）

**完整示例：快照生命周期**

```python
# 步骤 1: 从 Template 创建沙箱
sbx = Sandbox.create(template_id="tmpl_xxx")
print(f"Created sandbox: {sbx.sandbox_id}")

# 步骤 2: checkpoint Sandbox，得到可恢复 Template
template = sbx.checkpoint()
print(f"Template ID: {template.template_id}")

# 步骤 3: 从可恢复 Template 创建新沙箱
sbx2 = Sandbox.create(template_id=template.template_id)
print(f"Restored sandbox: {sbx2.sandbox_id}")
sbx2.delete()

sbx.delete()
```

**说明：**
- checkpoint 动作产生的 Template 保存沙箱的完整可恢复状态
- `checkpoint()` 不改变沙箱运行态
- 使用返回的 `template_id` 创建恢复后的沙箱
- 所有 Template 都使用 `tmpl_xxx` ID；可通过 `origin=checkpoint` 和 `boot_mode=resume` 识别其来源与启动能力
---

### 暂停、恢复和停止沙箱

```python
sandbox.suspend() -> bool
sandbox.resume() -> bool
sandbox.stop() -> bool
```

- `suspend()` 暂停运行中的沙箱。
- `resume()` 恢复已暂停的沙箱。
- `stop()` 停止沙箱运行时，但保留管理状态记录。如需完整删除记录并清理资源，请继续调用 `delete()`。

三个方法成功时返回 `True`，请求 conchd 失败时抛出 `RuntimeError`。

---

### 删除沙箱

```python
sandbox.delete(sandbox_id=None) -> bool
```

删除沙箱实例并释放资源。

**参数：**
- `sandbox_id` (可选): 删除指定的沙箱（默认删除当前实例）

**返回：** 成功返回 `True`，失败抛出 `RuntimeError`

**静态方法：**
```python
Sandbox.delete_sandbox(sandbox_id, unix_socket=None, api_url=None,
                       namespace=None, config_path=None) -> bool
```

无需创建实例即可删除指定沙箱。

**示例：**
```python
# 删除当前实例
with Sandbox.create(template_id="tmpl_xxx") as sbx:
    pass
# 自动删除（上下文管理器）

# 手动删除
sbx = Sandbox.create(template_id="tmpl_xxx")
sbx.delete()

# 直接删除指定沙箱
Sandbox.delete_sandbox("sandbox_abc")
```

---

## 文件操作

### 上传文件

```python
sandbox.files.upload(local_path, remote_path) -> WriteInfo | list[WriteInfo]
sandbox.files.upload(files) -> WriteInfo | list[WriteInfo]
sandbox.files.write(path, content) -> WriteInfo
sandbox.files.write_files(files) -> list[WriteInfo]
```

写入或上传文件到沙箱。返回对象包含 `name`、`path` 和 `type`，并直接使用 conch-init 返回的结果。推荐使用 `write()` / `write_files()`；`upload()` 保留用于上传本地文件路径。

**方式 1：上传本地文件**

```python
# 上传单个本地文件到沙箱
result = sbx.files.upload('./local.txt', '/home/user/remote.txt')
print(result.path)
```

**方式 2：写入内存内容**

```python
# 直接传入内容，无需本地文件
result = sbx.files.write('/home/user/a.txt', b'hello')
print(result.name)
```

**方式 3：批量上传**

```python
result = sbx.files.write_files([
    {"path": "/home/user/a.txt", "data": b"hello"},
    {"path": "/home/user/b.txt", "data": b"world"},
])
```

`write_files()` 每个条目使用 `{"path": remote_path, "data": content}`。`upload()` 兼容 `{"filepath": remote_path, "content": content}` 或 `{"local_path": local_path, "remote_path": remote_path}`。

---

### 下载文件

```python
sandbox.files.download(remote_path, local_path) -> dict
sandbox.files.read(remote_path, format="text") -> str
sandbox.files.read(remote_path, format="bytes") -> bytes
sandbox.files.read(remote_path, format="stream") -> Iterator[bytes]
```

从沙箱下载文件到本地。本地父目录不存在时会自动创建。远端文件不存在、认证失败、连接失败或其它 conch-init 错误会映射为 SDK 错误类型后抛出。

**示例：**
```python
# 从沙箱下载文件到本地
result = sbx.files.download('/home/user/output.txt', './downloaded.txt')
print(result)
# {'status': 0, 'size': 1024, 'message': 'OK'}

# 直接读取为文本
content = sbx.files.read('/home/user/output.txt')
print(content)

# 读取为 bytes
raw = sbx.files.read('/home/user/output.txt', format='bytes')
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
sandbox.files.list(path, depth=1) -> list[EntryInfo]
sandbox.files.search(path, pattern, exclude_patterns=None) -> list[EntryInfo]
```

列出或搜索沙箱内指定目录的文件。返回对象可直接访问 `name`、`path`、`type`、`size`、`permissions` 和 `modified_time`。

**参数：**
- `path` (str, 可选): 目录路径，不指定时列出当前目录

**示例：**
```python
# 列出沙箱当前目录所有文件
files = sbx.files.list('/home/user')
print(files)

# 搜索指定目录
files = sbx.files.search('/home/user', '*.py')
for item in files:
    print(item.path, item.size)
```

**返回值：** `list[EntryInfo]`，每个条目包含 `name`、`path`、`type`、`size`、`permissions`、`modified_time` 等字段。

---

## 健康检查

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
print(f"ID: {info.sandbox_id}, IP: {info.ip}, Source: {info.template_id}")
```

**返回值：** `SandboxInfo` 对象，参见 [数据类型](#sandboxinfo)。

---

## Sandbox 构造函数

```python
Sandbox(unix_socket=None, api_url=None, sandbox_id=None, template_id=None,
        namespace=None, vcpu_num=None, vcpu_max=None, ram_mb=None,
        config_path=None)
```

**主要参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `unix_socket` | str | Unix socket 路径，默认从配置文件读取 |
| `api_url` | str | 服务地址，仅当 `unix_socket` 为空时使用 |
| `sandbox_id` | str | 沙箱 ID，默认自动生成 |
| `template_id` | str | Template ID |
| `namespace` | str | 命名空间 |
| `vcpu_num` | int | 虚拟 CPU 数量 |
| `vcpu_max` | int | 虚拟 CPU 数量上限 |
| `ram_mb` | int | 内存大小（MB） |
| `config_path` | str | 配置文件路径，默认按优先级自动查找 |

**注意：** 构造函数仅初始化本地状态，不创建沙箱。请使用 `Sandbox.create()` 类方法。

---

## 数据类型

### SandboxInfo

```python
@dataclass
class SandboxInfo:
    sandbox_id: str
    ip: str
    template_id: Optional[str]
```

### TemplateInfo

```python
@dataclass
class TemplateInfo:
    template_id: str
    sandbox_id: str
```

### CommandResult

```python
class CommandResult:
    raw: dict        # 原始响应数据
    stdout: str      # 标准输出
    stderr: str      # 标准错误
    exit_code: int   # 退出码
    error: str       # 进程错误信息
    exited: bool     # 进程是否正常进入退出态（后台 wait 时由 end event 返回）
    process_status: str  # 进程状态文本（后台 wait 时由 end event 返回）
    logs: str        # 合并输出（stdout + stderr）
```

`str(result)` 返回合并输出（`logs.strip()`）。

### CommandExitException

```python
class CommandExitException(Exception):
    stdout: str
    stderr: str
    exit_code: int
    error: str
```

前台命令或 `CommandHandle.wait()` 非零退出时抛出。异常字符串会优先展示 `stderr`，若 `stderr` 为空则展示 `error` 字段。

### SDK 错误

conch-init RPC 错误会映射为 SDK 错误类型，避免直接暴露底层 Connect RPC 异常。

```python
class SandboxError(RuntimeError):
    pass

class InvalidArgumentError(SandboxError):
    pass

class NotFoundError(SandboxError):
    pass

class AuthenticationError(SandboxError):
    pass

```

例如 `sandbox.commands.connect(tag="missing")` 抛出 `NotFoundError`；`sandbox.commands.kill()` 不传 `pid/tag` 抛出 `InvalidArgumentError`；`sandbox.commands.kill(tag="missing")` 返回 `False`。

### ProcessInfo

```python
@dataclass
class ProcessInfo:
    pid: int
    tag: str | None
    cmd: str
    args: list[str]
    envs: dict[str, str]
    cwd: str | None
    running: bool
```

### 文件对象

```python
class FileType(Enum):
    FILE = "file"
    DIR = "dir"

@dataclass
class WriteInfo:
    name: str
    type: FileType | None
    path: str

@dataclass
class EntryInfo(WriteInfo):
    size: int
    permissions: str
    modified_time: str
    metadata: dict[str, str]
    is_directory: bool
```

`files.write()`、`files.upload()` 返回 `WriteInfo`；`files.write_files()` 返回 `list[WriteInfo]`；`files.list()` 和 `files.search()` 返回 `list[EntryInfo]`。

---

## 完整示例

### 示例 1: 基本使用（try-finally）

```python
from conch import Sandbox

sbx = None
try:
    sbx = Sandbox.create(template_id="tmpl_xxx")
    info = sbx.get_info()
    print(f"Created sandbox: {info.sandbox_id}, IP: {info.ip}")

    # 执行命令
    result = sbx.commands.run(cmd='python3', content='print("Hello!")')
    print(result.stdout)

    # 上传文件
    sbx.files.upload('./local.txt', '/home/user/remote.txt')

    # 下载文件
    sbx.files.download('/home/user/remote.txt', './downloaded.txt')

    # 列出文件
    files = sbx.files.list('/home/user')
    print(f"Files: {files}")
except (FileNotFoundError, ValueError, KeyError, RuntimeError) as e:
    print(f"Error: {e}")
finally:
    if sbx:
        sbx.delete()
```

### 示例 2: 基本使用（上下文管理器）

```python
from conch import Sandbox

with Sandbox.create(template_id="tmpl_xxx") as sbx:
    info = sbx.get_info()
    print(f"Created sandbox: {info.sandbox_id}, IP: {info.ip}")

    # 执行命令
    result = sbx.commands.run(cmd='python3', content='print("Hello!")')
    print(result.stdout)

    # 上传文件
    sbx.files.upload('./local.txt', '/home/user/remote.txt')

    # 下载文件
    sbx.files.download('/home/user/remote.txt', './downloaded.txt')

    # 列出文件
    files = sbx.files.list('/home/user')
    print(f"Files: {files}")
```

### 示例 3: Checkpoint 功能

```python
from conch import Sandbox

# 创建 checkpoint Template
sbx = Sandbox.create(template_id="tmpl_xxx")
template = sbx.checkpoint()
print(f"Created resumable template: {template.template_id}")

# 从可恢复 Template 启动
sbx2 = Sandbox.create(template_id=template.template_id)
sbx2.commands.run(cmd='python3', content='print("Restored!")')
sbx2.delete()
sbx.delete()
```

### 示例 4: 异常处理

```python
from conch import Sandbox

sbx = None
try:
    sbx = Sandbox.create(template_id="tmpl_xxx")
    result = sbx.commands.run(cmd='invalid_command')
except RuntimeError as e:
    print(f"Error: {e}")
finally:
    if sbx:
        sbx.delete()
```

---

## FAQ

### 为什么 `commands.run(cmd='echo', args=['$HOME'])` 输出的是 `$HOME` 而不是实际路径？

`commands.run()` 直接调用目标命令二进制，不经过 shell。`$HOME` 是 shell 变量语法，只有 shell 才会展开它。

**错误写法：**
```python
sbx.commands.run(cmd='echo', args=['$HOME'])
# 输出: $HOME（原样输出，echo 不做变量展开）
```

**正确写法：** 通过 `sh -c` 让 shell 执行：
```python
sbx.commands.run(cmd='sh', args=['-c', 'echo $HOME'])
# 输出: /root（shell 展开了变量）
```

同样的规则适用于管道、重定向、通配符等 shell 特性，都需要通过 `sh -c` 执行。
