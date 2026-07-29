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

## Sandbox 生命周期

### 上下文管理器

Sandbox 支持 Python 上下文管理器协议，提供更简洁的资源管理方式。

```python
from conch import Sandbox

with Sandbox.create(template_id="tmpl_xxx") as sbx:
    result = sbx.commands.run(cmd='python3', content='print("Hello")')
    print(result)
# 自动调用 delete()
```

---

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

### conchd 服务进程确认

```python
Sandbox.service_health(unix_socket=None, api_url=None, config_path=None) -> bool
```

当 `conchd` 确认其依赖的模块（`bbolt`, `containerd`, `daemon Client` 等）均就绪时返回 `True`。

### 获取沙箱（ `List` 和 `Get` ）

```python
Sandbox.list(namespace=None, state=None, limit=None, **connection_options) -> list[dict]
Sandbox.get(sandbox_id, namespace=None, **connection_options) -> Sandbox
```

`list` 的 `state` 筛选项可接受  `running` 和 `paused`，而 `limit` 需为 1-100 的整数。

**`Sandbox.list()` 参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `namespace` | str | 仅返回指定命名空间中的沙箱；未指定时使用 daemon 的默认命名空间 |
| `state` | list[str] | 按状态筛选；支持 `running` 和 `paused`，其中 `SUSPENDED` 和 `STOPPED` 均表示 `paused` |
| `limit` | int | 最多返回的沙箱数量，默认值为 `100`，取值范围为 `1` 至 `100` |
| `unix_socket` | str | conchd Unix socket 路径 |
| `api_url` | str | conchd HTTP API 地址；仅在未使用 Unix socket 时生效 |
| `config_path` | str | 未显式指定连接地址时用于查找连接配置的配置文件路径 |

**`Sandbox.get()` 参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `sandbox_id` | str | 要获取的沙箱 ID |
| `namespace` | str | 沙箱所属命名空间；未指定时使用 daemon 的默认命名空间 |
| `unix_socket` | str | conchd Unix socket 路径 |
| `api_url` | str | conchd HTTP API 地址；仅在未使用 Unix socket 时生效 |
| `config_path` | str | 未显式指定连接地址时用于查找连接配置的配置文件路径 |

`Sandbox.list()` 返回沙箱摘要字典列表；`Sandbox.get()` 返回已填充基础信息的 `Sandbox` 对象。沙箱响应可包含以下字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `templateID` | str | 创建沙箱所使用的 Template ID |
| `imageName` | str | 关联镜像名称；后端无法提供时为空字符串 |
| `snapshotID` | str | 关联快照 ID；后端无法提供时为空字符串 |
| `sandboxID` | str | 对外使用的 Conch 沙箱 ID |
| `namespace` | str | 沙箱所属命名空间 |
| `startedAt` | str | 沙箱创建时间，使用 RFC 3339 格式 |
| `endAt` | str | 沙箱停止时间；尚未停止时为空字符串 |
| `cpuCount` | int | 虚拟 CPU 数量 |
| `memoryMB` | int | 内存大小，单位为 MB |
| `diskSizeMB` | int | 磁盘大小，单位为 MB；后端无法提供时为 `0` |
| `conchInitVersion` | str | 沙箱 conch-init 版本；后端无法提供时为空字符串 |
| `alias` | str | 沙箱别名或名称 |
| `metadata` | dict | 沙箱元数据键值映射 |
| `volumeMounts` | list[dict] | 卷挂载列表，每项包含 `name` 和 `path` |



### 获取沙箱审计日志

```python
sandbox.logs(cursor=None, limit=None, direction=None, level=None, search=None) -> dict
```

**筛选参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `cursor` | str | 不透明分页游标，格式由服务端管理；首次查询时省略，后续查询使用上一次响应中的 `nextCursor` |
| `limit` | int | 最多返回的日志数量，默认值为 `1000`，取值范围为 `1` 至 `1000` |
| `direction` | str | 查询方向；`forward` 返回游标之后的日志，`backward` 返回游标之前的日志；省略游标的反向查询从最新日志开始 |
| `level` | str | 按日志级别精确筛选，例如 `info`、`warn` 或 `error`，不区分大小写 |
| `search` | str | 对日志消息进行不区分大小写的子字符串匹配，最大长度为 `256` 个字符 |

返回格式为 `{"logs": [...], "nextCursor": "..."}`。`nextCursor` 用于继续同一方向的分页查询。每条日志包含：

| 字段 | 类型 | 说明 |
|------|------|------|
| `timestamp` | str | 日志产生时间，使用 RFC 3339 格式 |
| `message` | str | 日志正文 |
| `level` | str | 日志严重级别，例如 `info`、`warn` 或 `error` |
| `fields` | dict[str, str] | 日志附加上下文，当前包含 `namespace` 和 `sandboxID` |

如果没有找到符合条件的日志，则返回 `{"logs": [], "nextCursor": ""}`。

### 更新沙箱网络策略

```python
sandbox.update_network(
    allow_out=None,
    deny_out=None,
    egress_proxy=None,
    rules=None,
    allow_internet_access=None,
) -> dict
```

**参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `allow_out` | list[str] | 允许访问的出站目标列表，对应请求字段 `allowOut` |
| `deny_out` | list[str] | 禁止访问的出站目标列表，对应请求字段 `denyOut` |
| `egress_proxy` | dict | 预留的出站代理配置；当前仅接受省略、`None` 或空字典，非空值将被拒绝 |
| `rules` | dict | 预留的自定义网络规则；当前仅接受省略、`None` 或空字典，非空值将被拒绝 |
| `allow_internet_access` | bool | 是否允许访问互联网；`True` 表示允许，`False` 表示禁止 |

该接口采用完整替换语义：每次调用都会替换全部出站网络配置。未传入的 `allow_out`、`deny_out`、`egress_proxy` 和 `rules` 将被清空；未传入 `allow_internet_access` 将恢复为默认的不限制状态，因此可能扩大网络访问范围。创建时设置的入站字段 `allowPublicTraffic` 和 `maskRequestHost` 不受此接口影响。更新成功时 daemon 返回 HTTP `204 No Content`，SDK 返回空字典 `{}`；请求无效或沙箱不存在时抛出 `RuntimeError`。

`egress_proxy` 和 `rules` 当前仅保持输入 schema 兼容，不提供对应的代理或规则执行功能。传入任何非空值时，create 和 update 请求都会返回 HTTP `400 Bad Request`。

---

### 获取沙箱信息

```python
sandbox.get_info() -> SandboxInfo
```

获取当前实例保存的沙箱 ID、IP 和源 Template ID。

**示例：**
```python
info = sbx.get_info()
print(f"ID: {info.sandbox_id}, IP: {info.ip}, Source: {info.template_id}")
```

**返回值：** `SandboxInfo` 对象，参见 [数据类型](#sandboxinfo)。

---

## 业务接口

### 执行命令

```python
sandbox.commands.run(cmd, args=None, cwd=None, env=None, content=None, background=False, tag=None, pty=None, stdin=None, timeout=None, on_stdout=None, on_stderr=None) -> CommandResult | CommandHandle
```

在沙箱中执行前台命令或启动后台进程。

**参数：**
- `cmd` (str): 命令名称（如 `python3`、`ls`、`sh`）
- `content` (str, 可选): 脚本内容
- `args` (list, 可选): 命令参数列表
- `cwd` (str, 可选): 执行目录，不指定时使用用户家目录
- `env` (dict, 可选): 环境变量，会追加到沙箱默认环境变量中
- `background` (bool, 可选): 为 `True` 时启动后台进程并返回 `CommandHandle`
- `tag` (str, 可选): 后台进程标签，可用于后续 `connect/list/kill`
- `pty` (dict, 可选): PTY 配置，例如 `{"cols": 80, "rows": 24}`
- `stdin` (str | bytes, 可选): 子进程启动时一次性写入标准输入的内容；写入后关闭标准输入
- `timeout` (float, 可选): 命令最长执行时间，单位秒；SDK 向 Agent 发送 `Connect-Timeout-Ms` 请求头
- `on_stdout` (callable, 可选): 前台命令 stdout 的增量回调
- `on_stderr` (callable, 可选): 前台命令 stderr 的增量回调

`content` 与 `args` 互斥；`stdin` 与 `pty` 互斥。

前台执行返回 `CommandResult`，后台启动返回 `CommandHandle`。非零退出抛 `CommandExitException`；超时抛 `TimeoutException`；参数错误抛 `InvalidArgumentError`。

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

# 一次性标准输入；result.stdout 为 'hello from stdin\n'
result = sbx.commands.run(
    cmd='python3',
    args=['-c', 'import sys; print(sys.stdin.read(), end="")'],
    stdin='hello from stdin\n',
)

# 超时单位为秒；Agent 收到 Connect-Timeout-Ms: 200
from conch import TimeoutException
try:
    sbx.commands.run(cmd='sleep', args=['10'], timeout=0.2)
except TimeoutException:
    print('timed out')

# 前台流式回调
chunks = []
result = sbx.commands.run(
    cmd='sh',
    args=['-c', 'printf foo; printf bar >&2'],
    on_stdout=lambda text: chunks.append(("stdout", text)),
    on_stderr=lambda text: chunks.append(("stderr", text)),
)
```

```python
handle = sbx.commands.run(
    cmd='python3',
    args=['-m', 'http.server', '18080'],
    cwd='/tmp',
    background=True,
    tag='http-srv',
)
```

---

### 连接后台进程

```python
sandbox.commands.connect(pid=None, tag=None) -> CommandHandle
command.wait() -> CommandResult
command.disconnect() -> None
```

通过 `pid` 或 `tag` 读取后台进程输出。

```python
command = sbx.commands.connect(tag='http-srv')
result = command.wait(on_stdout=lambda text: print(text, end=''))
```

`wait()` 在进程退出后返回；`disconnect()` 只关闭本次输出流。

---

### 列出后台进程

```python
sandbox.commands.list() -> list[ProcessInfo]
```

```python
for process in sbx.commands.list():
    print(process.pid, process.tag, process.running, process.exit_code)
```

---

### 终止后台进程

```python
sandbox.commands.kill(pid=None, tag=None, signal=15) -> bool
command.kill(signal=15) -> bool
```

通过 `pid` 或 `tag` 发送非零信号，默认 `15`；目标不存在返回 `False`。

```python
sbx.commands.kill(tag='http-srv', signal=15)
# 或：handle.kill(signal=15)
```

---

### 文件操作

#### 上传文件

```python
sandbox.files.upload(local_path, remote_path) -> WriteInfo | list[WriteInfo]
sandbox.files.upload(files) -> WriteInfo | list[WriteInfo]
sandbox.files.write(path, content) -> WriteInfo
sandbox.files.write_files(files) -> list[WriteInfo]
```

上传本地文件，或写入字符串、字节和文件流。

```python
# 上传单个本地文件到沙箱
result = sbx.files.upload('./local.txt', '/home/user/remote.txt')
print(result.path)
```

```python
# 直接传入内容，无需本地文件
result = sbx.files.write('/home/user/a.txt', b'hello')
print(result.name)
```

```python
result = sbx.files.write_files([
    {"path": "/home/user/a.txt", "data": b"hello"},
    {"path": "/home/user/b.txt", "data": b"world"},
])
```

`write_files()` 使用 `{"path": remote_path, "data": content}`；`upload()` 支持本地路径或内容规格。

---

#### 下载文件

```python
sandbox.files.download(remote_path, local_path) -> dict
sandbox.files.read(remote_path, format="text") -> str
sandbox.files.read(remote_path, format="bytes") -> bytes
sandbox.files.read(remote_path, format="stream") -> Iterator[bytes]
```

下载文件，或按文本、字节、流读取远端内容。

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

#### 列出文件

```python
sandbox.files.list(path, depth=1) -> list[EntryInfo]
sandbox.files.search(path, pattern, exclude_patterns=None) -> list[EntryInfo]
```

列出目录或按 glob 搜索文件。

**参数：**
- `path` (str): 目录路径
- `depth` (int, 可选): 列举深度，默认 `1`
- `pattern` (str): 搜索 glob 模式
- `exclude_patterns` (list[str], 可选): 搜索排除模式

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

返回 `list[EntryInfo]`。

---

### 健康检查

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

## Sandbox 构造函数

```python
Sandbox(unix_socket=None, api_url=None, sandbox_id=None, template_id=None,
        namespace=None, vcpu_num=None, vcpu_max=None, ram_mb=None,
        config_path=None, env=None, network=None)
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
| `env` | dict | 创建沙箱时传入的环境变量 |
| `network` | dict | 创建沙箱时传入的网络配置 |

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

class TimeoutException(SandboxError):
    pass

```

例如 `sandbox.commands.connect(tag="missing")` 抛出 `NotFoundError`；`sandbox.commands.kill()` 不传 `pid/tag` 抛出 `InvalidArgumentError`；`sandbox.commands.kill(tag="missing")` 返回 `False`；`commands.run(..., timeout=...)` 超时时抛出 `TimeoutException`。

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
