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

流式 RPC 使用 Connect streaming envelope，不是裸 JSON body。文档中的 streaming 示例展示单条 JSON message；直接 HTTP 调用时需要按 Connect 协议为每条 message 添加 5 字节 frame header。测试脚本或 Connect client 会负责 framing。

## 接口列表

| Interface | RPC Path | Description |
| --- | --- | --- |
| `runCommand` | `/pb.ProcessService/StartProcess` | 执行同步命令或启动后台进程 |
| `connectBackgroundProcess` | `/pb.ProcessService/Connect` | 连接后台进程并读取输出事件 |
| `listBackgroundProcesses` | `/pb.ProcessService/List` | 列出后台进程 |
| `killBackgroundProcess` | `/pb.ProcessService/SendSignal` | 向后台进程发送信号 |
| `uploadFile` | `/pb.FileService/PostFileStream` | 上传或写入文件 |
| `downloadFile` | `/pb.FileService/GetFileStream` | 下载或读取文件 |
| `listFiles` | `/pb.FileService/ListFiles` | 列出文件和目录 |
| `searchFiles` | `/pb.FileService/SearchFiles` | 按 glob 搜索文件 |

## Run Command

执行命令或脚本。`StartProcess` 是服务端流式 RPC，返回 `stream ProcessEvent`。使用 `background` 参数区分同步执行和后台进程。`content` 字段用于让 Agent 在 sandbox 内生成临时脚本并执行。

接口名称：`runCommand`

```text
pb.ProcessService/StartProcess
```

### SDK

同步执行：

```python
sandbox = Sandbox.create()

result = sandbox.commands.run(
    cmd="python3",
    args=["-c", "print('hello')"],
    cwd="/workspace",
    env={"DEMO_KEY": "demo-value"},
    background=False,
    pty=None,
)

print(result.stdout)
print(result.exit_code)
```

使用 `content` 执行脚本文本：

```python
script_result = sandbox.commands.run(
    cmd="python3",
    content="print('hello from content')",
    cwd="/workspace",
    background=False,
    pty=None,
)
```

后台执行：

```python
command = sandbox.commands.run(
    cmd="python3",
    args=["-m", "http.server", "18080"],
    cwd="/workspace",
    env={},
    background=True,
    tag="http-srv",
    pty=None,
)
```

### Direct Agent API

`StartProcess` 使用 Connect server-streaming 编码。下面展示请求 message；直接 HTTP 调用时该 message 需要使用 Connect streaming frame 编码，不能作为裸 JSON 直接 `curl --data`。

同步执行请求：

```json
{
  "cmd": "python3",
  "args": ["-c", "print(\"hello\")"],
  "cwd": "/workspace",
  "env": {
    "DEMO_KEY": "demo-value"
  },
  "content": "",
  "background": false,
  "pty": null
}
```

使用 `content` 请求：

```json
{
  "cmd": "python3",
  "args": [],
  "cwd": "/workspace",
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
  "cwd": "/workspace",
  "env": {},
  "content": "",
  "background": true,
  "tag": "http-srv",
  "pty": null
}
```

### Body Parameters

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `cmd` | string | required | 可执行命令，例如 `python3`、`sh` |
| `args` | string[] | optional | 命令参数 |
| `env` | object | optional | 本次执行追加环境变量 |
| `cwd` | string | optional | 工作目录；为空时使用当前用户 home 目录 |
| `content` | string | optional | 脚本文本内容。非空时 `args` 必须为空 |
| `background` | boolean | optional | 是否后台启动 |
| `tag` | string | optional | 后台进程标签 |
| `pty` | object/null | optional | PTY 配置；省略或 `null` 时不启用 |
| `pty.cols` | integer | optional | PTY 列数，未传或为 0 时使用默认值 |
| `pty.rows` | integer | optional | PTY 行数，未传或为 0 时使用默认值 |

`content` 不是 stdin。它用于执行一段脚本文本，例如 `cmd="python3"` 配合 `content="print('hello')"`。当前实现要求 `content` 和 `args` 互斥。

### Response Stream

返回类型：

```text
stream ProcessEvent
```

同步执行会发送输出事件和最终结束事件：

```json
{"data": {"stdout": "hello\n"}}
{"end": {"exitCode": 0, "exited": true, "status": "exited", "error": ""}}
```

后台执行先发送 `start` 事件，随后持续发送输出和最终 `end` 事件。SDK 的 `CommandHandle.wait()` 会消费启动时返回的同一个 stream，因此短命后台进程不会因为退出后无法重新 `Connect` 而丢失结果。后台输出通过内存事件队列转发，调用方需要及时消费 `StartProcess` 或 `Connect` 返回的 stream；如果进程持续大量输出而客户端长期不读取，过量输出事件可能被丢弃，但进程本身不会因此阻塞。

```json
{"start": {"pid": 23456}}
{"data": {"stdout": "ready\n"}}
{"end": {"exitCode": 0, "exited": true, "status": "exited", "error": ""}}
```

参数错误、命令启动失败和执行错误通过 `end.error` 表达。非零退出码不是 RPC 错误，`end.error` 为空，`end.exitCode` 为实际退出码。

## Connect Background Process

连接正在运行的后台进程并读取后续输出。通过 `process.pid` 或 `process.tag` 连接。

接口名称：`connectBackgroundProcess`

```text
pb.ProcessService/Connect
```

### SDK

```python
command = sandbox.commands.connect(tag="http-srv")
print(command)  # process handle (pid=42, tag=http-srv)

for stdout, stderr, pty in command:
    print(stdout or stderr or pty or "", end="")
```

### cURL / Direct Agent API

`Connect` 是服务端流式 RPC。下面展示单条 Connect JSON message；直接 HTTP 调用时该 message 需要使用 Connect streaming frame 编码，不能作为裸 JSON 直接 `curl --data`。

```json
{
  "process": {
    "tag": "http-srv"
  }
}
```

### Request Message

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `process.pid` | integer | optional | 进程 PID；与 `process.tag` 二选一 |
| `process.tag` | string | optional | 进程标签；与 `process.pid` 二选一 |

### Response

`stream ProcessEvent`

```json
{"start":{"pid":23456}}
{"data":{"stdout":"Serving HTTP on 0.0.0.0 port 18080 ...\n"}}
{"data":{"stderr":"warn\n"}}
{"data":{"pty":"interactive output\r\n"}}
{"end":{"exitCode":0,"exited":true,"status":"exited","error":""}}
```

## List Background Processes

列出 sandbox 内由 Agent 当前管理的后台进程，返回进程配置、PID 和标签。命令输出通过 `Connect` 流读取。

接口名称：`listBackgroundProcesses`

```text
pb.ProcessService/List
```

### SDK

```python
processes = sandbox.commands.list()
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

### Body Parameters

无入参字段。

### Response

```json
{
  "processes": [
    {
      "pid": 23456,
      "tag": "http-srv",
      "config": {
        "cmd": "python3",
        "args": ["-m", "http.server", "18080"],
        "env": {},
        "cwd": "/workspace",
        "pty": null
      },
      "running": true,
      "startedAt": "2026-07-11T10:00:00Z",
      "exitCode": -1,
      "finishedAt": ""
    }
  ]
}
```

## Kill Background Process

向后台进程发送信号。可通过 `pid` 或 `tag` 指定目标进程。调用方必须传入非 0 `signal`。

接口名称：`killBackgroundProcess`

```text
pb.ProcessService/SendSignal
```

### SDK

```python
ok = sandbox.commands.kill(tag="http-srv", signal=15)
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

### Body Parameters

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `process.pid` | integer | optional | 进程 PID；与 `process.tag` 二选一 |
| `process.tag` | string | optional | 进程标签；与 `process.pid` 二选一 |
| `signal` | integer | required | 信号编号，必须非 0；`15` 为 SIGTERM，`9` 为 SIGKILL |

### Response

```json
{}
```

错误通过 Connect status error 返回：缺少 selector 或 `signal=0` 返回 `InvalidArgument`，目标进程不存在返回 `NotFound`，目标进程已退出或信号发送时刚好结束返回 `FailedPrecondition`。

## Upload File

上传本地文件或写入内容到 sandbox。

接口名称：`uploadFile`

```text
pb.FileService/PostFileStream
```

### SDK

```python
# 写入文本内容
entry = sandbox.files.write("/workspace/remote.txt", "hello\n")
print(entry.path)

# 批量写入内容
entries = sandbox.files.write_files([
    {"path": "/workspace/a.txt", "data": "a"},
    {"path": "/workspace/b.txt", "data": b"b"},
])

# 上传本地文件
entry = sandbox.files.upload("local.txt", "/workspace/remote.txt")
```

### cURL / Direct Agent API

`PostFileStream` 是客户端流式 RPC。下面展示单条 `FileChunk` JSON message；直接 HTTP 调用时该 message 需要使用 Connect streaming frame 编码，不能作为裸 JSON 直接 `curl --data`。

```json
{
  "filepath": "/workspace/remote.txt",
  "content": "aGVsbG8K"
}
```

### Stream Message

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `filepath` | string | required on first chunk | sandbox 内目标路径；第一片必须传，后续分片可省略或传相同路径 |
| `content` | bytes | optional | 文件分片内容；JSON 示例中按 base64 表示，单片最大 1 MiB |

### Response

```json
{
  "uploadedCount": 1,
  "entries": [
    {
      "name": "remote.txt",
      "path": "/workspace/remote.txt",
      "type": "file"
    }
  ]
}
```

上传使用临时文件落盘，完整流接收后再 rename 到目标路径。上传失败通过 Connect status error 返回。

## Download File

从 sandbox 下载文件或读取文件内容。

接口名称：`downloadFile`

```text
pb.FileService/GetFileStream
```

### SDK

```python
content = sandbox.files.read("/workspace/remote.txt")
raw = sandbox.files.read("/workspace/remote.txt", format="bytes")

result = sandbox.files.download("/workspace/remote.txt", "remote.txt")
```

`read()` 默认按 UTF-8 文本返回；二进制内容请显式使用 `format="bytes"`。

### cURL / Direct Agent API

`GetFileStream` 是服务端流式 RPC。下面展示请求 JSON message；直接 HTTP 调用时该 message 需要使用 Connect streaming frame 编码，不能作为裸 JSON 直接 `curl --data`。

```json
{
  "filepath": "/workspace/remote.txt"
}
```

### Request Message

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `filepath` | string | required | sandbox 内源文件路径 |

### Response

`stream FileChunk`

响应为文件分片流，每片最大 1 MiB。第一片包含 `filepath`，后续分片通常只包含 `content`。Direct JSON 中 `content` 按 base64 表示。

## List Files

列出 sandbox 目录下的文件和目录。

接口名称：`listFiles`

```text
pb.FileService/ListFiles
```

### SDK

```python
items = sandbox.files.list("/workspace", depth=2)
for item in items:
    print(item.path, item.size)
```

### cURL

```bash
curl --request POST \
  --url "${AGENT_URL}/pb.FileService/ListFiles" \
  --header 'Connect-Protocol-Version: 1' \
  --header 'Content-Type: application/json' \
  --header "conch-init-token: ${AGENT_TOKEN}" \
  --data '{
    "path": "/workspace",
    "depth": 2
  }'
```

### Body Parameters

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `path` | string | required | 待列举路径 |
| `depth` | integer | optional | 递归深度；`1` 表示当前目录；省略、`0` 或负数按 `1` 处理 |

### Response

```json
{
  "entries": [
    {
      "name": "remote.txt",
      "path": "/workspace/remote.txt",
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

## Search Files

按 glob 模式搜索文件。

接口名称：`searchFiles`

```text
pb.FileService/SearchFiles
```

### SDK

```python
items = sandbox.files.search(
    path="/workspace",
    pattern="*.py",
    exclude_patterns=["*.bak"],
)
for item in items:
    print(item.path, item.type)
```

### cURL

```bash
curl --request POST \
  --url "${AGENT_URL}/pb.FileService/SearchFiles" \
  --header 'Connect-Protocol-Version: 1' \
  --header 'Content-Type: application/json' \
  --header "conch-init-token: ${AGENT_TOKEN}" \
  --data '{
    "path": "/workspace",
    "pattern": "*.py",
    "excludePatterns": ["*.bak"]
  }'
```

### Body Parameters

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `path` | string | required | 搜索根目录 |
| `pattern` | string | required | glob 匹配模式 |
| `excludePatterns` | string[] | optional | 排除模式 |

### Response

```json
{
  "entries": [
    {
      "name": "main.py",
      "path": "/workspace/main.py",
      "type": "file",
      "size": "128",
      "isDirectory": false,
      "permissions": "-rw-r--r--",
      "modifiedTime": "2026-07-11T10:00:00Z",
      "metadata": {}
    }
  ]
}
```
