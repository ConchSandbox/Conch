# Conch Python SDK API

## Sandbox

沙箱管理类，用于创建、管理和销毁沙箱环境。

### 初始化

```python
Sandbox(use_snapshot=False, api_url=None, client=None, workdir=None,
         sandbox_id=None, snapshot_id=None, vcpu_num=None, ram_mb=None,
         config_path=None)
```

**参数:**
- `use_snapshot`: bool - 是否使用快照创建沙箱（默认False）
- `api_url`: str - 沙箱API服务地址（默认从配置读取）
- `client`: AgentClient - 预存在的客户端连接
- `workdir`: str - 沙箱内工作目录路径
- `sandbox_id`: str - 沙箱唯一标识符（默认自动生成）
- `snapshot_id`: str - 快照ID
- `vcpu_num`: int - 虚拟CPU数量（默认从配置读取）
- `ram_mb`: int - 内存大小（MB，默认从配置读取）
- `config_path`: str - 配置文件路径

**注意:**
- `Sandbox()` 只初始化本地配置、工作目录和标识符，不会自动创建远端沙箱。
- 在调用 `execute()`、`upload()`、`download()`、`health_check()`、`list_files()` 之前，必须先调用 `create()` 或 `create_by_snapshot()`。
- 调用 `create()` 之前，请先确保 `conchd` 服务已经启动且 `sandbox.api_url` 可访问。

### 方法

#### create

基于镜像创建沙箱。

```python
create()
```

**返回:** `None` 表示成功，返回字符串表示创建失败原因

#### create_by_snapshot

基于快照创建沙箱。

```python
create_by_snapshot()
```

**返回:** `None` 表示成功，返回字符串表示创建失败原因

#### execute

在沙箱中执行命令。

```python
execute(cmd, content=None, cwd=None, **kwargs)
```

**参数:**
- `cmd`: str - 要执行的命令
- `content`: str - 要写入沙箱的脚本内容
- `cwd`: str - 执行目录（默认为工作目录）
- `args`: list - 命令参数列表
- `env`: dict - 环境变量字典
- `timeout`: int - 超时时间（秒）
- `user`: str - 执行用户
- `filename`: str - 脚本文件名（默认推断）

**返回:** Execution对象，包含stdout、stderr、exit_code等信息

**前置条件:** 必须先成功调用 `create()` 或 `create_by_snapshot()`

#### delete

删除沙箱。

```python
delete()
```

**返回:** dict - 删除结果状态

#### pause

暂停沙箱并创建快照。

```python
pause()
```

**返回:** dict - 暂停结果和快照信息

#### health_check

检查沙箱健康状态。

```python
health_check()
```

**返回:** dict - 健康状态信息

#### upload

上传文件到沙箱工作目录。

```python
upload(local_path, remote_path)
# 或
upload([{"filepath": "path/to/file", "content": b"bytes"}, ...])
```

**参数:**
- `local_path`: str - 本地文件路径
- `remote_path`: str - 沙箱内目标路径
- 或传入文件规范列表

**返回:** dict - 上传结果状态

#### download

从沙箱下载文件。

```python
download(remote_path, local_path)
```

**参数:**
- `remote_path`: str - 沙箱内文件路径
- `local_path`: str - 本地保存路径

**返回:** dict - 下载结果状态

#### list_files

列出沙箱工作目录中的所有文件。

```python
list_files()
```

**返回:** list - 文件路径列表

#### get_sandbox_id

获取沙箱ID。

```python
get_sandbox_id()
```

**返回:** str - 沙箱唯一标识符

#### get_snapshot_id

获取快照ID。

```python
get_snapshot_id()
```

**返回:** str - 快照唯一标识符

## AgentClient

沙箱代理客户端，用于与沙箱内代理服务通信。

### 初始化

```python
AgentClient(host, port=4064)
```

**参数:**
- `host`: str - 沙箱IP地址
- `port`: int - gRPC端口（默认4064）

### 方法

#### health_check

健康检查。

```python
health_check()
```

**返回:** dict - 状态信息

#### start_process

启动进程并执行命令。

```python
start_process(cmd, cwd=None, env=None, content=None, filename=None, args=None)
```

**参数:**
- `cmd`: str - 命令
- `cwd`: str - 工作目录
- `env`: dict - 环境变量
- `content`: str - 脚本内容
- `filename`: str - 文件名
- `args`: list - 参数列表

**返回:** dict - 执行结果（包含stdout、stderr、exit_code等）

#### post_files

批量上传文件。

```python
post_files(local_path, remote_path)
# 或
post_files([{"filepath": "path", "content": b"bytes"}])
# 或
post_files(files=[{"filepath": "path", "content": b"bytes"}])
```

**返回:** dict - 上传结果

#### get_file

下载单个文件。

```python
get_file(remote_path, local_path)
```

**返回:** dict - 下载结果

#### get_files

批量下载文件。

```python
get_files([{"remote": "path", "local": "path"}])
```

**参数:**
- `mappings`: list - 路径映射列表

**返回:** dict - 批量下载结果

#### close

关闭连接。

```python
close()
```

## Execution

执行结果对象。

### 属性

- `stdout`: str - 标准输出
- `stderr`: str - 标准错误
- `exit_code`: int - 退出码
- `logs`: str - 合并的输出日志

### 使用示例

```python
from conch import Sandbox

box = Sandbox()
err = box.create()
if err:
    raise RuntimeError(f"failed to create sandbox: {err}")

try:
    # 执行命令
    result = box.execute(cmd='python3', content='print("Hello")')
    print(result.stdout)

    # 上传文件
    box.upload('local.txt', 'remote.txt')

    # 下载文件
    box.download('remote.txt', 'downloaded.txt')

    # 列出文件
    files = box.list_files()
finally:
    box.delete()
```
