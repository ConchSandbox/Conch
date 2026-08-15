# Conch Python SDK API

## 快速开始

创建 Sandbox 前，需要先启动 `conchd`，并准备一个完整的 Template。如果尚未创建，参见 [Conch Template 与镜像指南](template.md)。

在终端中输入 `python3` 进入交互环境：

```pycon
>>> from conch import Sandbox
>>> sandbox = Sandbox.create(template_id="<template-id>")
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

也可使用 `with Sandbox.create(template_id="<template-id>") as sandbox:` 上下文管理器，自动调用 `delete()`。

---

## Sandbox 生命周期

### 上下文管理器

Sandbox 支持 Python 上下文管理器协议，提供更简洁的资源管理方式。

```python
from conch import Sandbox

with Sandbox.create(template_id="<template-id>") as sbx:
    result = sbx.commands.run(cmd='python3', content='print("Hello")')
    print(result)
# 自动调用 delete()
```

---

### 创建沙箱

```text
Sandbox.create(template_id=None, sandbox_id=None,
               vcpu_num=None, vcpu_max=None, ram_mb=None,
               volume_mounts=None, env=None, network=None,
               vmm_name=None) -> Sandbox
```

基于 Template 创建沙箱。省略字段时，由 conchd 使用
`sandbox.default_spec`；`template_id` 和默认 Template 都为空时，conchd 返回 HTTP 400。

**参数：**
- `template_id` (str, 可选): 要启动的 Template ID
- `sandbox_id` (str, 可选): 指定沙箱 ID，默认自动生成
- `vcpu_num` / `vcpu_max` / `ram_mb` (int, 可选): 沙箱资源配置
- `volume_mounts` (list, 可选): 卷挂载配置
- `env` (dict[str, str], 可选): 创建沙箱时传入的环境变量。键不能为空且不能包含 `=` 或 NUL，值不能包含 NUL。沙箱 ID、访问令牌、协议字段、网络配置和序列化后的环境变量共同组成初始化消息，该消息按 UTF-8 字节计算不得超过 16 KiB；明显超限的环境会在虚拟机启动前拒绝，完整消息会在发送前再次校验。
- `network` (dict, 可选): 创建时应用的 IP 级网络策略。支持 `allowOut`、`denyOut`、`allowIn`、`denyIn` 和 `allow_internet_access`。
- `vmm_name` (str, 可选): 指定 VMM，例如 `stratovirt`、`cloud-hypervisor`；省略时使用 conchd 的 `sandbox.backend`。该名称须在 conchd 的 `sandbox` 配置段中存在，否则返回 HTTP 400。

控制面请求失败时，SDK 继续抛出 `RuntimeError`（或现有子类）。当 conchd 返回结构化错误时，异常文本为 `<code>: <error>`，例如 `sandbox.invalid_environment: invalid sandbox environment`。其中 `code` 是可供自动化稳定判断的错误码；`error` 是面向用户的文案，不保证跨版本不变。旧服务端的纯文本错误响应仍会原样显示。

**返回：** 成功返回 `Sandbox` 对象。

**异常：** 请求 conchd 失败时抛出 `RuntimeError`。

**示例：**
```python
# 从指定 Template 创建
sbx = Sandbox.create(template_id="<template-id>")
sbx.commands.run(cmd='python3', content='print("Hello")')
sbx.delete()

# 省略资源时，使用 sandbox.default_spec
sbx = Sandbox.create(template_id="<template-id>")
sbx.delete()

# 从 checkpoint 产生的可恢复 Template 创建
sbx = Sandbox.create(template_id="<template-id>")
sbx.commands.run(cmd='python3', content='print("Restored")')
sbx.delete()

# 使用上下文管理器
with Sandbox.create(template_id="<template-id>") as sbx:
    sbx.commands.run(cmd='python3', content='print("Hello")')
```

---

### Checkpoint Sandbox

```text
sandbox.checkpoint() -> TemplateInfo
```

捕获沙箱当前状态并返回一个可恢复 Template。Checkpoint 是作用于 Sandbox 的动作，不是独立资源；该动作不会停止或删除原沙箱。

**返回：** `TemplateInfo` 对象（包含 `template_id` 和 `sandbox_id`）

**完整示例：快照生命周期**

```python
# 步骤 1: 从 Template 创建沙箱
sbx = Sandbox.create(template_id="<template-id>")
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
- 可通过 `origin=checkpoint` 和 `boot_mode=resume` 识别 Template 的来源与启动能力

---

### 暂停和恢复沙箱

```text
sandbox.suspend() -> bool
sandbox.resume() -> bool
```

- `suspend()` 暂停运行中的沙箱。
- `resume()` 恢复已暂停的沙箱。

两个方法成功时返回 `True`，请求 conchd 失败时抛出 `RuntimeError`。SDK 没有只停止运行时并保留管理记录的 `stop()` 方法；需要释放 Sandbox 资源时使用 `delete()`。

---

### 删除沙箱

```text
sandbox.delete(sandbox_id=None) -> bool
```

删除沙箱实例并释放资源。

**参数：**
- `sandbox_id` (可选): 删除指定的沙箱（默认删除当前实例）

**返回：** 成功返回 `True`，失败抛出 `RuntimeError`

**静态方法：**
```text
Sandbox.delete_sandbox(sandbox_id) -> bool
```

无需创建实例即可删除指定沙箱。

**示例：**
```python
# 删除当前实例
with Sandbox.create(template_id="<template-id>") as sbx:
    pass
# 自动删除（上下文管理器）

# 手动删除
sbx = Sandbox.create(template_id="<template-id>")
sbx.delete()

# 直接删除指定沙箱
Sandbox.delete_sandbox("sandbox_abc")
```

### conchd 服务进程确认

```text
Sandbox.service_health() -> bool
```

当 `conchd` 的状态存储、containerd host、daemon client 和 runtime service 等核心组件已完成初始化时返回 `True`。该检查仅确认组件已初始化，不会主动探测各依赖的实时运行状态。

### 获取沙箱（ `List` 和 `Get` ）

```text
Sandbox.list(state=None, limit=None) -> list[dict]
Sandbox.get(sandbox_id) -> Sandbox
```

`list` 的 `state` 筛选项可接受 `running` 和 `paused`，而 `limit` 需为 1-5000 的整数。未指定 `state` 时也只返回 `READY` 和 `SUSPENDED` 记录；`UNKNOWN` 等内部状态不会出现在列表中。

**`Sandbox.list()` 参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `state` | list[str] | 按状态筛选；支持 `running` 和 `paused`，其中 `READY` 表示 `running`，`SUSPENDED` 表示 `paused` |
| `limit` | int | 最多返回的沙箱数量，默认值为 `100`，取值范围为 `1` 至 `5000` |

**`Sandbox.get()` 参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `sandbox_id` | str | 要获取的沙箱 ID |

`Sandbox.list()` 返回沙箱摘要字典列表；`Sandbox.get()` 返回已填充基础信息的 `Sandbox` 对象。沙箱响应可包含以下字段：

`Sandbox.get()` 会填充资源、domain、metadata 和 lifecycle 等可用的控制面字段。由于 daemon 当前不会恢复创建时的 conch-init 访问令牌，GET 响应会省略 `conchInitAccessToken`，该方法返回的对象仅用于控制面操作，例如读取元数据或删除沙箱。命令、文件和沙箱内 Agent 健康检查会抛出 `Agent credentials unavailable for retrieved sandbox`。`Sandbox.create()` 返回的对象不受此限制。

下表使用 REST API 的 JSON 字段名。Python SDK 会将其映射为 Python 属性，例如 `sandboxID` 对应 `sandbox_id`、`templateID` 对应 `template_id`、`startedAt` 对应 `started_at`，`domain` 对应 `ip`。

| 字段 | 类型 | 说明 |
|------|------|------|
| `templateID` | str | 创建沙箱所使用的 Template ID |
| `imageName` | str | 关联镜像名称；后端无法提供时为空字符串 |
| `snapshotID` | str | 关联快照 ID；后端无法提供时为空字符串 |
| `sandboxID` | str | 对外使用的 Conch 沙箱 ID |
| `startedAt` | str | 沙箱创建时间，使用 RFC 3339 格式 |
| `endAt` | str | 预留的沙箱结束时间字段；当前固定返回空字符串 |
| `cpuCount` | int | 虚拟 CPU 数量 |
| `memoryMB` | int | 内存大小，单位为 MB |
| `diskSizeMB` | int | 磁盘大小，单位为 MB；后端无法提供时为 `0` |
| `conchInitVersion` | str | 沙箱 conch-init 版本；后端无法提供时为空字符串 |
| `alias` | str | 沙箱别名或名称 |
| `domain` | str | 沙箱当前可用的网络地址；详细 GET 响应提供 |
| `metadata` | dict | 沙箱元数据键值映射 |
| `lifecycle` | dict | 生命周期配置；当前包含 `autoResume` 占位字段 |
| `network` | dict | 当前持久化的 IP 级网络策略；详细 GET 响应提供 |
| `volumeMounts` | list[dict] | 预留的卷挂载列表；当前固定返回空列表 |
---

### 更新沙箱网络策略

```python
sandbox.update_network(
    allow_out=None,
    deny_out=None,
    allow_in=None,
    deny_in=None,
    allow_internet_access=None,
) -> bool
```

该方法通过 `PUT /api/v1/sandboxes/{sandboxID}/network` 完整替换沙箱的网络策略。省略的列表按空列表处理，省略 `allow_internet_access` 表示不额外禁止未匹配的出站流量。`READY` 和 `SUSPENDED` 状态的沙箱均可更新。

| 参数 | JSON 字段 | 说明 |
|------|-----------|------|
| `allow_out` | `allowOut` | 允许访问的出站 IPv4 地址或 CIDR |
| `deny_out` | `denyOut` | 拒绝访问的出站 IPv4 地址或 CIDR |
| `allow_in` | `allowIn` | 允许进入 guest 的来源 IPv4 地址或 CIDR |
| `deny_in` | `denyIn` | 拒绝进入 guest 的来源 IPv4 地址或 CIDR |
| `allow_internet_access` | `allow_internet_access` | 为 `False` 时拒绝未被其他规则接受的出站流量；为 `True` 或省略时不增加该默认拒绝 |

规则语义如下：

- 对新连接，显式拒绝规则优先于显式允许规则。更新策略前已经建立且仍由 conntrack 跟踪的连接不会被立即终止，可以继续到连接关闭或跟踪记录过期。
- 非空允许列表启用白名单模式，拒绝该方向上未匹配的流量。
- 只有拒绝列表时，拒绝匹配地址并允许未匹配流量。
- 允许和拒绝列表都为空时，该方向不受列表限制。
- `allow_internet_access: false` 会额外拒绝未匹配的出站流量，不影响入站规则。
- 当前仅接受 IPv4 地址和 IPv4 CIDR；四个列表合计最多 1024 项。

出站规则挂载在 Linux network namespace 内从 guest tap 发出的转发路径，入站规则挂载在转发到 guest tap 的路径。入站规则只过滤平台已经路由到该沙箱的 IP 流量；它不会创建主机监听端口、公开服务、执行 hostname 路由或修改 HTTP 请求。

---

### 获取沙箱信息

```text
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

```text
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
    cmd='sh',
    args=['-c', 'echo started; sleep 10; echo finished'],
    background=True,
    tag='short-job',
)
```

---

### 连接后台进程

```text
sandbox.commands.connect(pid=None, tag=None) -> CommandHandle
command.wait() -> CommandResult
command.disconnect() -> None
```

通过 `pid` 或 `tag` 读取后台进程输出。

```python
command = sbx.commands.connect(tag='short-job')
result = command.wait(on_stdout=lambda text: print(text, end=''))
```

`wait()` 在进程退出后返回；`disconnect()` 只关闭本次输出流。

---

### 列出后台进程

```text
sandbox.commands.list() -> list[ProcessInfo]
```

```python
for process in sbx.commands.list():
    print(process.pid, process.tag, process.running, process.exit_code)
```

---

### 终止后台进程

```text
sandbox.commands.kill(pid=None, tag=None, signal=15) -> bool
command.kill(signal=15) -> bool
```

通过 `pid` 或 `tag` 发送非零信号，默认 `15`；目标不存在返回 `False`。

```python
sbx.commands.kill(tag='short-job', signal=15)
# 或：handle.kill(signal=15)
```

---

### 文件操作

所有文件接口的远端路径（`path`、`remote_path` 和上传规格中的 `filepath`）必须是已规范化的
guest 绝对路径，例如 `/home/user/a.txt` 或卷在 guest 内可见的 `/workspace/data.txt`。
相对路径、`..` 或 `.` 路径段、重复或多余的分隔符以及 NUL 字节都会在文件访问前被拒绝；
根路径 `/` 本身有效。`guestd` 在 chroot 到 sandbox merge root 后提供这些接口，因此 `/`
表示 guest 根目录，已配置的卷仍可通过其 guest 挂载目标访问。该校验定义的是 guest API 的
路径语义，不表示此前存在越过 chroot 边界的 host 文件系统逃逸。

#### 上传文件

```text
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

```text
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

```text
sandbox.files.list(path, depth=1) -> list[EntryInfo]
```

列出目录中的文件和子目录。

**参数：**
- `path` (str): 目录路径
- `depth` (int, 可选): 列举深度，默认 `1`

**示例：**
```python
# 列出沙箱当前目录所有文件
files = sbx.files.list('/home/user')
print(files)
```

返回 `list[EntryInfo]`。

#### 搜索文件

```text
sandbox.files.search(path, pattern, exclude_patterns=None) -> list[EntryInfo]
```

按 glob 模式搜索文件。

**参数：**
- `path` (str): 搜索目录路径
- `pattern` (str): 搜索 glob 模式
- `exclude_patterns` (list[str], 可选): 搜索排除模式

**示例：**
```python
# 搜索指定目录中的 Python 文件
files = sbx.files.search('/home/user', '*.py')
for item in files:
    print(item.path, item.size)
```

返回 `list[EntryInfo]`。

---

### 健康检查

```text
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
Sandbox(sandbox_id=None, template_id=None,
        vcpu_num=None, vcpu_max=None, ram_mb=None,
        volume_mounts=None, env=None, network=None,
        vmm_name=None)
```

**主要参数：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `sandbox_id` | str | 沙箱 ID，默认自动生成 |
| `template_id` | str | Template ID |
| `vcpu_num` | int | 虚拟 CPU 数量 |
| `vcpu_max` | int | 虚拟 CPU 数量上限 |
| `ram_mb` | int | 内存大小（MB） |
| `volume_mounts` | list | 创建沙箱时使用的卷挂载配置 |
| `env` | dict | 创建沙箱时传入的环境变量 |
| `network` | dict | 创建时应用的 IP 级网络策略 |
| `vmm_name` | str | 指定 VMM，省略时由 conchd 决定 |

**注意：** 构造函数仅初始化本地状态，不创建沙箱。请使用 `Sandbox.create()` 类方法创建沙箱。

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
    started_at: str
    exit_code: int
    finished_at: str
    stdout: str
    stderr: str
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
    sbx = Sandbox.create(template_id="<template-id>")
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

with Sandbox.create(template_id="<template-id>") as sbx:
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
sbx = Sandbox.create(template_id="<template-id>")
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
    sbx = Sandbox.create(template_id="<template-id>")
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
