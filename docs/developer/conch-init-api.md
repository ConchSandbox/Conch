# conch-init Agent API

本文说明 `conch-init` 在 sandbox 内暴露的 Agent API。该接口用于命令执行、后台进程、信号发送和文件传输，当前由 `conch-init` 在 sandbox 内监听 `:4064`。

## 协议和鉴权

Agent API 使用 Connect RPC over h2c。普通 unary RPC 可通过 Connect HTTP/JSON 调用：

```text
Connect-Protocol-Version: 1
Content-Type: application/json
conch-init-token: <agent_token>
```

`conch-init-token` 是 sandbox 内 Agent API 的访问凭证。缺失、未初始化或错误时返回 Connect `Unauthenticated`。`GET /health` 是普通 HTTP 健康检查，不要求该 header。

```bash
curl --request GET \
  --url "${AGENT_URL}/health"
```

返回：

```json
{"status":"OK","message":"OK"}
```

`ProcessDataEvent.stdout`、`stderr` 和 `pty` 在 protobuf 中是 `bytes`。Connect JSON 使用 base64 表示这些字段；Python SDK 会对每个输出通道执行增量 UTF-8 解码，因此 SDK 示例中看到的是普通字符串。

## 接口列表

| 接口名称 | RPC 路径 | 说明 |
| --- | --- | --- |
| `StartProcess` | `/pb.ProcessService/StartProcess` | 执行同步命令或启动后台进程 |
| `Connect` | `/pb.ProcessService/Connect` | 连接后台进程并读取输出事件 |
| `List` | `/pb.ProcessService/List` | 列出后台进程 |
| `SendSignal` | `/pb.ProcessService/SendSignal` | 向后台进程发送信号 |
| `PostFileStream` | `/pb.FileService/PostFileStream` | 上传或写入文件 |
| `GetFileStream` | `/pb.FileService/GetFileStream` | 下载或读取文件 |
| `ListFiles` | `/pb.FileService/ListFiles` | 列出文件和目录 |
| `SearchFiles` | `/pb.FileService/SearchFiles` | 按 glob 搜索文件 |

## 执行命令

RPC 名称：`StartProcess`

```text
pb.ProcessService/StartProcess
```

### 入参

| 字段 | 类型 | 是否必填 | 说明 |
| --- | --- | --- | --- |
| `cmd` | string | 必填 | 可执行命令，例如 `python3`、`sh` |
| `args` | string[] | 可选 | 命令参数 |
| `env` | object | 可选 | 本次执行追加环境变量 |
| `cwd` | string | 可选 | 工作目录；为空时使用当前用户 home 目录 |
| `content` | string | 可选 | 脚本内容；非空时 `args` 必须为空 |
| `background` | boolean | 可选 | 是否后台启动 |
| `tag` | string | 可选 | 后台进程标签 |
| `pty` | object/null | 可选 | PTY 配置；省略或 `null` 时不启用 |
| `pty.cols` | integer | 可选 | PTY 列数，未传或为 0 时使用默认值 |
| `pty.rows` | integer | 可选 | PTY 行数，未传或为 0 时使用默认值 |
| `stdin` | bytes | 可选 | 一次性写入子进程标准输入的字节；Connect JSON 中使用 base64 |

`content` 与 `args` 互斥；`stdin` 与 `pty` 互斥。

`Connect-Timeout-Ms` 指定进程最长执行时间，单位为毫秒。值为 `0` 或缺失时不设置超时。

### 出参

```text
stream ProcessEvent
```

```jsonl
{"start": {"pid": 23456}}
{"data": {"stdout": "aGVsbG8K"}}
{"end": {"exitCode": 0, "exited": true, "status": "exited", "error": ""}}
```

`stdout`、`stderr` 和 `pty` 为 base64 编码的 bytes。进程启动前的请求错误通过 Connect status error 返回；命令的非零退出码通过 `end.exitCode` 返回。

### 说明

执行命令或脚本。`background` 区分前台执行和后台进程。

### SDK

同步执行：

```python
from conch import Sandbox

sandbox = Sandbox.create(template_name="<template_name>")

result = sandbox.commands.run(
    cmd="python3",
    args=["-c", "print('hello')"],
    cwd="/tmp",
    env={"DEMO_KEY": "demo-value"},
    background=False,
    pty=None,
)

print(result.stdout, end="")
print(result.exit_code)
```

也可以改用 `Sandbox.create(template_id="sha256:<digest>")` 直接按 Template ID
创建；`template_name` 和 `template_id` 不能同时指定。

输出：

```text
hello
0
```

使用 `content` 执行脚本文本：

```python
script_result = sandbox.commands.run(
    cmd="python3",
    content="print('hello from content')",
    cwd="/tmp",
    background=False,
    pty=None,
)

print(script_result.stdout, end="")
```

传入标准输入和执行超时：

```python
from conch import TimeoutException

stdin_result = sandbox.commands.run(
    cmd="python3",
    args=["-c", "import sys; print(sys.stdin.buffer.read().hex())"],
    stdin=b"\x00\xff",
)
print(stdin_result.stdout, end="")  # 00ff

try:
    sandbox.commands.run(cmd="sleep", args=["10"], timeout=0.2)
except TimeoutException:
    print("timed out")
```

输出：

```text
hello from content
00ff
timed out
```

后台执行：

```python
command = sandbox.commands.run(
    cmd="python3",
    args=["-m", "http.server", "18080"],
    cwd="/tmp",
    env={},
    background=True,
    tag="http-srv",
    pty=None,
    timeout=0,
)

print(command)
```

> 注意：这是长时间运行的 HTTP 服务。启动后不要直接对返回的 `CommandHandle` 调用 `wait()`；`wait()` 会一直等到进程退出，而 HTTP 服务通常不会自行退出。需要读取启动日志时，应使用 `connect()` 配合有限输出读取，并在结束时调用 `disconnect()`；需要停止服务时再调用 `kill()`。

`timeout` 的单位为秒。对于长时间运行的服务，设置为 `0` 表示不限制最长存活时间；服务结束时应通过 `kill()` 显式停止。设置为正数时，超时后 `conch-init` 会终止进程。

输出中的 PID 由系统动态分配：

```text
process handle (pid=204, tag=http-srv)
```

### 直接调用 Agent API

同步执行请求：

```json
{
  "cmd": "python3",
  "args": ["-c", "import sys; print(sys.stdin.read(), end='')"],
  "cwd": "/tmp",
  "env": {
    "DEMO_KEY": "demo-value"
  },
  "content": "",
  "background": false,
  "pty": null,
  "stdin": "aGVsbG8K"
}
```

`stdin` 是 protobuf `bytes` 字段，因此 Connect JSON 使用 base64；上例的
`aGVsbG8K` 解码后是 `hello\n`。Agent 在启动子进程时写入该内容，并在写入后关闭标准输入。

使用 `content` 请求：

```json
{
  "cmd": "python3",
  "args": [],
  "cwd": "/tmp",
  "env": {
    "DEMO_KEY": "demo-value"
  },
  "content": "import os\nprint(\"hello from content\")\nprint(os.environ.get(\"DEMO_KEY\", \"\"))",
  "background": false,
  "pty": null
}
```

后台执行请求：

```json
{
  "cmd": "python3",
  "args": ["-m", "http.server", "18080"],
  "cwd": "/tmp",
  "env": {},
  "content": "",
  "background": true,
  "tag": "http-srv",
  "pty": null
}
```

后台执行先发送 `start`，随后持续发送输出和最终 `end`。调用方需要及时消费 `StartProcess` 或 `Connect` 返回的 stream；过量后台输出事件可能被丢弃，但进程不会因此阻塞。超时会终止前台和后台进程，前台调用返回 Connect `DeadlineExceeded`。

## 连接后台进程

RPC 名称：`Connect`

```text
pb.ProcessService/Connect
```

### 入参

| 字段 | 类型 | 是否必填 | 说明 |
| --- | --- | --- | --- |
| `process.pid` | integer | 可选 | 进程 PID；与 `process.tag` 二选一 |
| `process.tag` | string | 可选 | 进程标签；与 `process.pid` 二选一 |

### 出参

```text
stream ProcessEvent
```

```jsonl
{"start":{"pid":23456}}
{"data":{"stdout":"aGVhcnRiZWF0IDEK"}}
{"data":{"stderr":"d2Fybgo="}}
{"data":{"pty":"aW50ZXJhY3RpdmUgb3V0cHV0DQo="}}
{"end":{"exitCode":0,"exited":true,"status":"exited","error":""}}
```

### 说明

连接正在运行的后台进程并读取后续输出。通过 `process.pid` 或 `process.tag` 连接。

### SDK

下面使用持续输出心跳的进程说明长时间连接。`background=True` 会立即返回后台进程句柄，`timeout=0` 表示不限制进程运行时间。由于该进程不会自行退出，不要对这个句柄调用 `wait()`，否则程序会一直阻塞。只启动进程时，保存返回的句柄即可；需要读取后续输出时，再通过 `connect()` 建立连接。

```python
starter = sandbox.commands.run(
    cmd="python3",
    content="""import time
time.sleep(1)  # 让 Connect 有机会先建立订阅
print("service-ready", flush=True)
count = 0
while True:
    count += 1
    print(f"heartbeat {count}", flush=True)
    time.sleep(1)
""",
    background=True,
    tag="heartbeat-worker",
    timeout=0,
)
print(starter)  # process handle (pid=42, tag=heartbeat-worker)
```

需要查看长时间连接的输出时，将日志读取放到后台线程，主线程负责停止控制。连接建立前已经产生的历史输出不会保证再次返回。

```python
import threading

command = sandbox.commands.connect(tag="heartbeat-worker")

def read_logs():
    for stdout, stderr, pty in command:
        if stdout:
            print(stdout, end="")
        if stderr:
            print(stderr, end="")

reader = threading.Thread(target=read_logs, daemon=True)
reader.start()
try:
    input("Press Enter or Ctrl-C to stop: ")
except KeyboardInterrupt:
    pass
finally:
    command.disconnect()
    sandbox.commands.kill(tag="heartbeat-worker", signal=2)
    reader.join(timeout=1)
```

`disconnect()` 只关闭日志连接，不会终止进程；`kill()` 才会停止进程。如果进程不响应，再使用 `signal=9` 强制终止。

`stdout` 的事件分片不保证与日志行一一对应，调用方应按自身协议累积和解析输出。`disconnect()` 只关闭当前客户端的 `Connect` 输出流，不会终止后台进程。长服务只有在退出或收到信号后才会在流中发送 `end` 事件；此后 `CommandHandle.wait()` 才返回最终的退出码。

### 请求示例

```json
{
  "process": {
    "tag": "heartbeat-worker"
  }
}
```

三个 data 字段均为 base64 编码的 bytes。SDK callback 接收到的是增量 UTF-8 解码后的字符串。

## 列出后台进程

RPC 名称：`List`

```text
pb.ProcessService/List
```

### 入参

无入参字段。

### 出参

```json
{
  "processes": [
    {
      "pid": 23456,
      "tag": "http-srv",
      "config": {"cmd": "python3", "args": ["-m", "http.server", "18080"], "env": {}, "cwd": "/tmp", "pty": null},
      "running": true,
      "startedAt": "2026-07-11T10:00:00Z",
      "exitCode": -1,
      "finishedAt": ""
    }
  ]
}
```

### 说明

列出 sandbox 内由 Agent 当前管理的后台进程。命令输出通过 `Connect` 流读取。

### SDK

```python
processes = sandbox.commands.list()
for process in processes:
    print(process.pid, process.tag, process.running, process.cmd, process.args)
```

实际输出示例：

```text
204 docs-background True /bin/sh ['-c', 'sleep 1; echo background-ready']
```

### cURL

```bash
curl --request POST \
  --url "${AGENT_URL}/pb.ProcessService/List" \
  --header 'Connect-Protocol-Version: 1' \
  --header 'Content-Type: application/json' \
  --header "conch-init-token: ${AGENT_TOKEN}" \
  --data '{}'
```

## 发送进程信号

RPC 名称：`SendSignal`

```text
pb.ProcessService/SendSignal
```

### 入参

| 字段 | 类型 | 是否必填 | 说明 |
| --- | --- | --- | --- |
| `process.pid` | integer | 可选 | 进程 PID；与 `process.tag` 二选一 |
| `process.tag` | string | 可选 | 进程标签；与 `process.pid` 二选一 |
| `signal` | integer | 必填 | 信号编号，必须非 0；`15` 为 SIGTERM，`9` 为 SIGKILL |

### 出参

```json
{}
```

### 说明

向后台进程发送信号。目标进程不存在时返回 Connect `NotFound`。

### SDK

```python
ok = sandbox.commands.kill(tag="http-srv", signal=15)
print(ok)
```

输出：

```text
True
```

不存在的目标返回 `False`：

```python
print(sandbox.commands.kill(tag="does-not-exist", signal=15))
```

```text
False
```

SDK 成功发送信号返回 `True`，目标进程不存在时返回 `False`；其它错误会映射为 SDK 错误类型后抛出。

### cURL

```bash
curl --request POST \
  --url "${AGENT_URL}/pb.ProcessService/SendSignal" \
  --header 'Connect-Protocol-Version: 1' \
  --header 'Content-Type: application/json' \
  --header "conch-init-token: ${AGENT_TOKEN}" \
  --data '{
    "process": {
      "tag": "http-srv"
    },
    "signal": 15
  }'
```

## 上传文件

RPC 名称：`PostFileStream`

```text
pb.FileService/PostFileStream
```

### 入参

| 字段 | 类型 | 是否必填 | 说明 |
| --- | --- | --- | --- |
| `filepath` | string | 首个分片必填 | sandbox 内目标路径；第一片必须传，后续分片可省略或传相同路径 |
| `content` | bytes | 可选 | 文件分片内容；JSON 示例中按 base64 表示，单片最大 1 MiB |

### 出参

```json
{
  "uploadedCount": 1,
  "entries": [{"name": "remote.txt", "path": "/tmp/conch-doc-api/remote.txt", "type": "file"}]
}
```

### 说明

上传本地文件或写入内容到 sandbox。

### SDK

```python
# 写入文本内容
entry = sandbox.files.write("/tmp/conch-doc-api/remote.txt", "hello\n")
print(entry)

# 批量写入内容
entries = sandbox.files.write_files([
    {"path": "/tmp/conch-doc-api/a.txt", "data": "a"},
    {"path": "/tmp/conch-doc-api/main.py", "data": b"print('ok')\n"},
])
print(entries)

# 上传本地文件
entry = sandbox.files.upload("local.txt", "/tmp/conch-doc-api/uploaded.txt")
```

`write()` 和 `write_files()` 的实际输出：

```text
WriteInfo(name='remote.txt', type=<FileType.FILE: 'file'>, path='/tmp/conch-doc-api/remote.txt')
[WriteInfo(name='a.txt', type=<FileType.FILE: 'file'>, path='/tmp/conch-doc-api/a.txt'), WriteInfo(name='main.py', type=<FileType.FILE: 'file'>, path='/tmp/conch-doc-api/main.py')]
```

### 请求示例

```json
{
  "filepath": "/tmp/conch-doc-api/remote.txt",
  "content": "aGVsbG8K"
}
```

上传使用临时文件落盘，完整流接收后再 rename 到目标路径。上传失败通过 Connect status error 返回。

## 下载文件

RPC 名称：`GetFileStream`

```text
pb.FileService/GetFileStream
```

### 入参

| 字段 | 类型 | 是否必填 | 说明 |
| --- | --- | --- | --- |
| `filepath` | string | 必填 | sandbox 内源文件路径 |

### 出参

```text
stream FileChunk
```

每片最大 1 MiB。第一片包含 `filepath`，后续分片通常只包含 `content`。

### 说明

从 sandbox 下载文件或读取文件内容。

### SDK

```python
content = sandbox.files.read("/tmp/conch-doc-api/remote.txt")
raw = sandbox.files.read("/tmp/conch-doc-api/remote.txt", format="bytes")
chunks = sandbox.files.read("/tmp/conch-doc-api/remote.txt", format="stream")

result = sandbox.files.download("/tmp/conch-doc-api/remote.txt", "remote.txt")

print(repr(content))
print(repr(raw))
print(repr(b"".join(chunks)))
print(result)
```

`read()` 默认按 UTF-8 文本返回；二进制内容请显式使用 `format="bytes"`。

输出：

```text
'hello\n'
b'hello\n'
b'hello\n'
{'status': 0, 'size': 6, 'message': 'OK'}
```

### 请求示例

```json
{
  "filepath": "/tmp/conch-doc-api/remote.txt"
}
```

## 列出文件

RPC 名称：`ListFiles`

```text
pb.FileService/ListFiles
```

### 入参

| 字段 | 类型 | 是否必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 必填 | 待列举路径 |
| `depth` | integer | 可选 | 递归深度；`1` 表示当前目录；省略、`0` 或负数按 `1` 处理 |

### 出参

```json
{
  "entries": [
    {
      "name": "remote.txt",
      "path": "/tmp/conch-doc-api/remote.txt",
      "type": "file",
      "size": "6",
      "isDirectory": false,
      "permissions": "-rw-r--r--",
      "modifiedTime": "2026-07-11T10:00:00Z",
      "metadata": {}
    }
  ]
}
```

### 说明

列出 sandbox 目录下的文件和目录。

### SDK

```python
items = sandbox.files.list("/tmp/conch-doc-api", depth=2)
for item in items:
    print(item.path, item.type, item.size, item.is_directory)
```

假设 `linked` 是指向 `target` 的目录符号链接，输出示例为：

```text
/tmp/conch-doc-api/a.txt FileType.FILE 1 False
/tmp/conch-doc-api/linked FileType.DIR 60 True
/tmp/conch-doc-api/linked/main.py FileType.FILE 12 False
/tmp/conch-doc-api/remote.txt FileType.FILE 6 False
/tmp/conch-doc-api/target FileType.DIR 60 True
/tmp/conch-doc-api/target/main.py FileType.FILE 12 False
```

目录符号链接会作为目录返回，并在 `depth` 允许时沿链接的逻辑路径继续列举。实现会检测当前递归链中的真实路径，避免符号链接环导致无限遍历；失效符号链接仍作为普通条目返回。

PID、时间戳和目录条目的 `size` 由运行时环境决定，示例值不应作为固定断言；文件内容大小可以稳定断言。

### cURL

```bash
curl --request POST \
  --url "${AGENT_URL}/pb.FileService/ListFiles" \
  --header 'Connect-Protocol-Version: 1' \
  --header 'Content-Type: application/json' \
  --header "conch-init-token: ${AGENT_TOKEN}" \
  --data '{
    "path": "/tmp/conch-doc-api",
    "depth": 2
  }'
```

## 搜索文件

RPC 名称：`SearchFiles`

```text
pb.FileService/SearchFiles
```

### 入参

| 字段 | 类型 | 是否必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 必填 | 搜索根目录 |
| `pattern` | string | 必填 | glob 匹配模式 |
| `excludePatterns` | string[] | 可选 | 排除模式 |

### 出参

```json
{
  "entries": [
    {
      "name": "main.py",
      "path": "/tmp/conch-doc-api/target/main.py",
      "type": "file",
      "size": "12",
      "isDirectory": false,
      "permissions": "-rw-r--r--",
      "modifiedTime": "2026-07-11T10:00:00Z",
      "metadata": {}
    }
  ]
}
```

### 说明

按 glob 模式搜索文件。

### SDK

```python
items = sandbox.files.search(
    path="/tmp/conch-doc-api",
    pattern="*.py",
    exclude_patterns=["*.bak"],
)
for item in items:
    print(item.path, item.type)
```

目录符号链接也会参与递归搜索。实际输出示例：

```text
/tmp/conch-doc-api/linked/main.py FileType.FILE
/tmp/conch-doc-api/target/main.py FileType.FILE
```

### cURL

```bash
curl --request POST \
  --url "${AGENT_URL}/pb.FileService/SearchFiles" \
  --header 'Connect-Protocol-Version: 1' \
  --header 'Content-Type: application/json' \
  --header "conch-init-token: ${AGENT_TOKEN}" \
  --data '{
    "path": "/tmp/conch-doc-api",
    "pattern": "*.py",
    "excludePatterns": ["*.bak"]
  }'
```
