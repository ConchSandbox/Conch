from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Iterable as _Iterable, Mapping as _Mapping, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ConnectProcessRequest(_message.Message):
    __slots__ = ["process"]
    PROCESS_FIELD_NUMBER: _ClassVar[int]
    process: ProcessSelector
    def __init__(self, process: _Optional[_Union[ProcessSelector, _Mapping]] = ...) -> None: ...

class FileChunk(_message.Message):
    __slots__ = ["content", "filepath"]
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    FILEPATH_FIELD_NUMBER: _ClassVar[int]
    content: bytes
    filepath: str
    def __init__(self, filepath: _Optional[str] = ..., content: _Optional[bytes] = ...) -> None: ...

class FileEntry(_message.Message):
    __slots__ = ["is_directory", "metadata", "modified_time", "name", "path", "permissions", "size", "type"]
    class MetadataEntry(_message.Message):
        __slots__ = ["key", "value"]
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    IS_DIRECTORY_FIELD_NUMBER: _ClassVar[int]
    METADATA_FIELD_NUMBER: _ClassVar[int]
    MODIFIED_TIME_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    PERMISSIONS_FIELD_NUMBER: _ClassVar[int]
    SIZE_FIELD_NUMBER: _ClassVar[int]
    TYPE_FIELD_NUMBER: _ClassVar[int]
    is_directory: bool
    metadata: _containers.ScalarMap[str, str]
    modified_time: str
    name: str
    path: str
    permissions: str
    size: int
    type: str
    def __init__(self, name: _Optional[str] = ..., path: _Optional[str] = ..., type: _Optional[str] = ..., size: _Optional[int] = ..., permissions: _Optional[str] = ..., modified_time: _Optional[str] = ..., metadata: _Optional[_Mapping[str, str]] = ..., is_directory: bool = ...) -> None: ...

class GetFileRequest(_message.Message):
    __slots__ = ["filepath"]
    FILEPATH_FIELD_NUMBER: _ClassVar[int]
    filepath: str
    def __init__(self, filepath: _Optional[str] = ...) -> None: ...

class ListFilesRequest(_message.Message):
    __slots__ = ["depth", "path"]
    DEPTH_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    depth: int
    path: str
    def __init__(self, path: _Optional[str] = ..., depth: _Optional[int] = ...) -> None: ...

class ListFilesResponse(_message.Message):
    __slots__ = ["entries"]
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    entries: _containers.RepeatedCompositeFieldContainer[FileEntry]
    def __init__(self, entries: _Optional[_Iterable[_Union[FileEntry, _Mapping]]] = ...) -> None: ...

class ListProcessesRequest(_message.Message):
    __slots__ = []
    def __init__(self) -> None: ...

class ListProcessesResponse(_message.Message):
    __slots__ = ["processes"]
    PROCESSES_FIELD_NUMBER: _ClassVar[int]
    processes: _containers.RepeatedCompositeFieldContainer[ProcessInfo]
    def __init__(self, processes: _Optional[_Iterable[_Union[ProcessInfo, _Mapping]]] = ...) -> None: ...

class PTY(_message.Message):
    __slots__ = ["cols", "rows"]
    COLS_FIELD_NUMBER: _ClassVar[int]
    ROWS_FIELD_NUMBER: _ClassVar[int]
    cols: int
    rows: int
    def __init__(self, cols: _Optional[int] = ..., rows: _Optional[int] = ...) -> None: ...

class PostFilesResponse(_message.Message):
    __slots__ = ["entries", "error", "uploaded_count"]
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    UPLOADED_COUNT_FIELD_NUMBER: _ClassVar[int]
    entries: _containers.RepeatedCompositeFieldContainer[WriteInfo]
    error: str
    uploaded_count: int
    def __init__(self, uploaded_count: _Optional[int] = ..., error: _Optional[str] = ..., entries: _Optional[_Iterable[_Union[WriteInfo, _Mapping]]] = ...) -> None: ...

class ProcessConfig(_message.Message):
    __slots__ = ["args", "cmd", "cwd", "env", "pty"]
    class EnvEntry(_message.Message):
        __slots__ = ["key", "value"]
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    ARGS_FIELD_NUMBER: _ClassVar[int]
    CMD_FIELD_NUMBER: _ClassVar[int]
    CWD_FIELD_NUMBER: _ClassVar[int]
    ENV_FIELD_NUMBER: _ClassVar[int]
    PTY_FIELD_NUMBER: _ClassVar[int]
    args: _containers.RepeatedScalarFieldContainer[str]
    cmd: str
    cwd: str
    env: _containers.ScalarMap[str, str]
    pty: PTY
    def __init__(self, cmd: _Optional[str] = ..., args: _Optional[_Iterable[str]] = ..., env: _Optional[_Mapping[str, str]] = ..., cwd: _Optional[str] = ..., pty: _Optional[_Union[PTY, _Mapping]] = ...) -> None: ...

class ProcessDataEvent(_message.Message):
    __slots__ = ["pty", "stderr", "stdout"]
    PTY_FIELD_NUMBER: _ClassVar[int]
    STDERR_FIELD_NUMBER: _ClassVar[int]
    STDOUT_FIELD_NUMBER: _ClassVar[int]
    pty: str
    stderr: str
    stdout: str
    def __init__(self, stdout: _Optional[str] = ..., stderr: _Optional[str] = ..., pty: _Optional[str] = ...) -> None: ...

class ProcessEndEvent(_message.Message):
    __slots__ = ["error", "exit_code", "exited", "status"]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    EXITED_FIELD_NUMBER: _ClassVar[int]
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    error: str
    exit_code: int
    exited: bool
    status: str
    def __init__(self, exit_code: _Optional[int] = ..., exited: bool = ..., status: _Optional[str] = ..., error: _Optional[str] = ...) -> None: ...

class ProcessEvent(_message.Message):
    __slots__ = ["data", "end", "keepalive", "start"]
    DATA_FIELD_NUMBER: _ClassVar[int]
    END_FIELD_NUMBER: _ClassVar[int]
    KEEPALIVE_FIELD_NUMBER: _ClassVar[int]
    START_FIELD_NUMBER: _ClassVar[int]
    data: ProcessDataEvent
    end: ProcessEndEvent
    keepalive: ProcessKeepAlive
    start: ProcessStartEvent
    def __init__(self, start: _Optional[_Union[ProcessStartEvent, _Mapping]] = ..., data: _Optional[_Union[ProcessDataEvent, _Mapping]] = ..., end: _Optional[_Union[ProcessEndEvent, _Mapping]] = ..., keepalive: _Optional[_Union[ProcessKeepAlive, _Mapping]] = ...) -> None: ...

class ProcessInfo(_message.Message):
    __slots__ = ["config", "exit_code", "finished_at", "pid", "running", "started_at", "stderr", "stdout", "tag"]
    CONFIG_FIELD_NUMBER: _ClassVar[int]
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    FINISHED_AT_FIELD_NUMBER: _ClassVar[int]
    PID_FIELD_NUMBER: _ClassVar[int]
    RUNNING_FIELD_NUMBER: _ClassVar[int]
    STARTED_AT_FIELD_NUMBER: _ClassVar[int]
    STDERR_FIELD_NUMBER: _ClassVar[int]
    STDOUT_FIELD_NUMBER: _ClassVar[int]
    TAG_FIELD_NUMBER: _ClassVar[int]
    config: ProcessConfig
    exit_code: int
    finished_at: str
    pid: int
    running: bool
    started_at: str
    stderr: str
    stdout: str
    tag: str
    def __init__(self, pid: _Optional[int] = ..., tag: _Optional[str] = ..., config: _Optional[_Union[ProcessConfig, _Mapping]] = ..., running: bool = ..., started_at: _Optional[str] = ..., exit_code: _Optional[int] = ..., finished_at: _Optional[str] = ..., stdout: _Optional[str] = ..., stderr: _Optional[str] = ...) -> None: ...

class ProcessKeepAlive(_message.Message):
    __slots__ = []
    def __init__(self) -> None: ...

class ProcessSelector(_message.Message):
    __slots__ = ["pid", "tag"]
    PID_FIELD_NUMBER: _ClassVar[int]
    TAG_FIELD_NUMBER: _ClassVar[int]
    pid: int
    tag: str
    def __init__(self, pid: _Optional[int] = ..., tag: _Optional[str] = ...) -> None: ...

class ProcessStartEvent(_message.Message):
    __slots__ = ["pid"]
    PID_FIELD_NUMBER: _ClassVar[int]
    pid: int
    def __init__(self, pid: _Optional[int] = ...) -> None: ...

class SearchFilesRequest(_message.Message):
    __slots__ = ["exclude_patterns", "path", "pattern"]
    EXCLUDE_PATTERNS_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    PATTERN_FIELD_NUMBER: _ClassVar[int]
    exclude_patterns: _containers.RepeatedScalarFieldContainer[str]
    path: str
    pattern: str
    def __init__(self, path: _Optional[str] = ..., pattern: _Optional[str] = ..., exclude_patterns: _Optional[_Iterable[str]] = ...) -> None: ...

class SearchFilesResponse(_message.Message):
    __slots__ = ["entries"]
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    entries: _containers.RepeatedCompositeFieldContainer[FileEntry]
    def __init__(self, entries: _Optional[_Iterable[_Union[FileEntry, _Mapping]]] = ...) -> None: ...

class SendSignalRequest(_message.Message):
    __slots__ = ["process", "signal"]
    PROCESS_FIELD_NUMBER: _ClassVar[int]
    SIGNAL_FIELD_NUMBER: _ClassVar[int]
    process: ProcessSelector
    signal: int
    def __init__(self, process: _Optional[_Union[ProcessSelector, _Mapping]] = ..., signal: _Optional[int] = ...) -> None: ...

class SendSignalResponse(_message.Message):
    __slots__ = []
    def __init__(self) -> None: ...

class StartProcessRequest(_message.Message):
    __slots__ = ["args", "background", "cmd", "content", "cwd", "env", "pty", "tag"]
    class EnvEntry(_message.Message):
        __slots__ = ["key", "value"]
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    ARGS_FIELD_NUMBER: _ClassVar[int]
    BACKGROUND_FIELD_NUMBER: _ClassVar[int]
    CMD_FIELD_NUMBER: _ClassVar[int]
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    CWD_FIELD_NUMBER: _ClassVar[int]
    ENV_FIELD_NUMBER: _ClassVar[int]
    PTY_FIELD_NUMBER: _ClassVar[int]
    TAG_FIELD_NUMBER: _ClassVar[int]
    args: _containers.RepeatedScalarFieldContainer[str]
    background: bool
    cmd: str
    content: str
    cwd: str
    env: _containers.ScalarMap[str, str]
    pty: PTY
    tag: str
    def __init__(self, cmd: _Optional[str] = ..., args: _Optional[_Iterable[str]] = ..., env: _Optional[_Mapping[str, str]] = ..., cwd: _Optional[str] = ..., content: _Optional[str] = ..., background: bool = ..., tag: _Optional[str] = ..., pty: _Optional[_Union[PTY, _Mapping]] = ...) -> None: ...

class WriteInfo(_message.Message):
    __slots__ = ["name", "path", "type"]
    NAME_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    TYPE_FIELD_NUMBER: _ClassVar[int]
    name: str
    path: str
    type: str
    def __init__(self, name: _Optional[str] = ..., path: _Optional[str] = ..., type: _Optional[str] = ...) -> None: ...
