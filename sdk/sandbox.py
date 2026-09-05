import os
import math
from enum import Enum
from io import IOBase, TextIOBase
from typing import IO, Callable, Iterator, Optional, Dict, Any, List, Tuple, Literal, overload, TypedDict, Union
from dataclasses import dataclass, field
from functools import lru_cache
from urllib.parse import quote, quote_plus
import requests
import requests_unixsocket
import uuid
import secrets

# Try relative imports first (when imported as a package), fall back to absolute imports
from .client import AgentClient
from .errors import InvalidArgumentError, NotFoundError, SandboxError


# API keys
SANDBOX_ID_KEY = "sandbox_id"
TEMPLATE_NAME_KEY = "template_name"
TEMPLATE_ID_KEY = "template_id"
STATUS_KEY = "status"
MESSAGE_KEY = "message"

TEMPLATE_ID_RESP_KEY = "template_id"
VOLUME_MOUNTS_KEY = "volumeMounts"


# Control plane endpoint: conchd listens on this local Unix socket by default
DEFAULT_UNIX_SOCKET = "/var/run/conch/conchd.sock"

# API paths
SANDBOX_COLLECTION_PATH = "/api/v1/sandboxes"
SANDBOX_INSTANCE_PATH_TEMPLATE = "/api/v1/sandboxes/{sandbox_id}"
SANDBOX_CREATE_PATH = SANDBOX_COLLECTION_PATH
SANDBOX_LIST_PATH = SANDBOX_COLLECTION_PATH
SANDBOX_GET_PATH_TEMPLATE = SANDBOX_INSTANCE_PATH_TEMPLATE
SANDBOX_DELETE_PATH_TEMPLATE = SANDBOX_INSTANCE_PATH_TEMPLATE
SANDBOX_NETWORK_PATH_TEMPLATE = "/api/v1/sandboxes/{sandbox_id}/network"
SANDBOX_SUSPEND_PATH = "/api/sandbox/suspend"
SANDBOX_RESUME_PATH = "/api/sandbox/resume"
SANDBOX_CHECKPOINT_PATH = "/api/sandbox/checkpoint"
SERVICE_HEALTH_PATH = "/health"
HTTP_NO_CONTENT = 204

RANDOM_ID_HEX_BYTES = 12
UNKNOWN_EXIT_CODE = -1

def generate_random_id(prefix: str = "sandbox_") -> str:
    return prefix + secrets.token_hex(RANDOM_ID_HEX_BYTES)


@lru_cache(maxsize=1)
def warn_onproxy():
    """Warn once per process: a proxy usually breaks local conchd requests."""
    if os.environ.get('http_proxy') or os.environ.get('HTTP_PROXY'):
        print('warning: http proxy enabled')
    if os.environ.get('https_proxy') or os.environ.get('HTTPS_PROXY'):
        print('warning: https proxy enabled')


def _request_exception_message(exc: requests.exceptions.RequestException) -> str:
    response = getattr(exc, "response", None)
    if response is not None:
        try:
            body = response.json()
        except (ValueError, TypeError):
            body = None
        if isinstance(body, dict) and body.get("error"):
            code = body.get("code")
            return f"{code}: {body['error']}" if code else str(body["error"])
        if getattr(response, "text", None):
            return response.text.strip()
    return str(exc)

@dataclass
class TemplateInfo:
    template_name: str
    template_id: str
    sandbox_id: str


@dataclass
class SandboxInfo:
    # TODO: Extend this with more sandbox metadata once the SDK surface is finalized.
    sandbox_id: str
    ip: str
    template_name: Optional[str]
    template_id: Optional[str]


Stdout = str
Stderr = str
PtyOutput = str
OutputHandler = Callable[[str], None]


class CommandResult:
    def __init__(self, data: Optional[Dict[str, Any]] = None):
        data = data or {}
        self.raw = data
        self.stdout = data.get("stdout", "")
        self.stderr = data.get("stderr", "")
        self.exit_code = data.get("exit_code", UNKNOWN_EXIT_CODE)
        self.error = data.get("error", "")
        self.exited = data.get("exited")
        self.process_status = data.get("process_status", data.get("status_text", ""))
        self.logs = self.stdout + self.stderr

    def __str__(self):
        return self.logs.strip()


class CommandExitException(Exception):
    def __init__(self, stdout: str, stderr: str, exit_code: int, error: Optional[str] = None):
        self.stdout = stdout
        self.stderr = stderr
        self.exit_code = exit_code
        self.error = error or ""
        super().__init__(str(self))

    def __str__(self):
        detail = self.stderr or self.error
        if detail:
            return f"Command exited with code {self.exit_code} and error:\n{detail}"
        return f"Command exited with code {self.exit_code}"


Execution = CommandResult


class FileType(Enum):
    FILE = "file"
    DIR = "dir"


def _map_file_type(type_name: Optional[str]) -> Optional[FileType]:
    normalized = (type_name or "").strip().lower()
    if normalized == "file":
        return FileType.FILE
    if normalized in {"dir", "directory"}:
        return FileType.DIR
    return None


@dataclass
class WriteInfo:
    name: str
    type: Optional[FileType]
    path: str

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "WriteInfo":
        path = data.get("path", "")
        return cls(
            name=data.get("name") or os.path.basename(path.rstrip("/")) or path,
            type=_map_file_type(data.get("type")),
            path=path,
        )


@dataclass
class EntryInfo(WriteInfo):
    size: int = 0
    permissions: str = ""
    modified_time: str = ""
    metadata: Dict[str, str] = field(default_factory=dict)
    is_directory: bool = False

    def __post_init__(self):
        if self.metadata is None:
            self.metadata = {}

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "EntryInfo":
        path = data.get("path", "")
        name = data.get("name") or os.path.basename(path.rstrip("/")) or path
        is_directory = bool(data.get("isDirectory", data.get("is_directory", False)))
        return cls(
            name=name,
            type=_map_file_type(data.get("type")) or (FileType.DIR if is_directory else None),
            path=path,
            size=int(data.get("size", 0) or 0),
            permissions=data.get("permissions", ""),
            modified_time=data.get("modifiedTime", data.get("modified_time", "")),
            metadata=dict(data.get("metadata") or {}),
            is_directory=is_directory,
        )


class WriteEntry(TypedDict):
    path: str
    data: Union[str, bytes, IO]


@dataclass
class ProcessInfo:
    pid: int
    tag: Optional[str] = None
    cmd: str = ""
    args: List[str] = field(default_factory=list)
    envs: Dict[str, str] = field(default_factory=dict)
    cwd: Optional[str] = None
    running: bool = False
    started_at: str = ""
    exit_code: int = 0
    finished_at: str = ""
    stdout: str = ""
    stderr: str = ""

    def __post_init__(self):
        if self.args is None:
            self.args = []
        if self.envs is None:
            self.envs = {}

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "ProcessInfo":
        config = data.get("config") or {}
        return cls(
            pid=data.get("pid", 0),
            tag=data.get("tag") or None,
            cmd=config.get("cmd", ""),
            args=list(config.get("args", [])),
            envs=dict(config.get("env", config.get("envs", {}))),
            cwd=config.get("cwd") or None,
            running=bool(data.get("running", False)),
            started_at=data.get("startedAt", ""),
            exit_code=data.get("exitCode", 0),
            finished_at=data.get("finishedAt", ""),
            stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""),
        )


@dataclass
class ProcessData:
    stdout: str = ""
    stderr: str = ""
    pty: str = ""


class ProcessEvent:
    def __init__(self, data: Dict[str, Any]):
        self.raw = data
        self.start = data.get("start")
        self.end = data.get("end")
        self.keepalive = data.get("keepalive")
        payload = data.get("data") or {}
        self.data = ProcessData(
            stdout=payload.get("stdout", ""),
            stderr=payload.get("stderr", ""),
            pty=payload.get("pty", ""),
        ) if payload else None

    def __str__(self) -> str:
        if self.start:
            pid = self.start.get("pid", "")
            return f"process started: pid={pid}" if pid else "process started"
        if self.data:
            return self.data.stdout or self.data.stderr or self.data.pty
        if self.end:
            exit_code = self.end.get("exitCode", self.end.get("exit_code", UNKNOWN_EXIT_CODE))
            status = self.end.get("status", "")
            error = self.end.get("error", "")
            details = [f"exit_code={exit_code}"]
            if status:
                details.append(f"status={status}")
            if error:
                details.append(f"error={error}")
            return "process ended: " + ", ".join(details)
        if self.keepalive:
            return "process keepalive"
        return "process event"

    def __repr__(self) -> str:
        return str(self)


class CommandHandle:
    def __init__(
            self,
            sandbox: "Sandbox",
            process: Dict[str, Any],
            events: Optional[Iterator[Dict[str, Any]]] = None,
    ):
        self._sandbox = sandbox
        self.process = ProcessInfo.from_dict(process)
        self.pid = self.process.pid
        self.tag = self.process.tag or ""
        self._events = events
        self._stdout: str = ""
        self._stderr: str = ""
        self._result: Optional[CommandResult] = None
        self._iteration_exception: Optional[Exception] = None

    def __iter__(self) -> Iterator[Tuple[Optional[Stdout], Optional[Stderr], Optional[PtyOutput]]]:
        return self._handle_events()

    def __str__(self) -> str:
        selector = []
        if self.pid is not None:
            selector.append(f"pid={self.pid}")
        if self.tag:
            selector.append(f"tag={self.tag}")
        return f"process handle ({', '.join(selector)})" if selector else "process handle"

    def __repr__(self) -> str:
        return str(self)

    def _event_source(self) -> Iterator[Dict[str, Any]]:
        if self._events is not None:
            events = self._events
            self._events = None
            return iter(events)
        raise SandboxError("CommandHandle has no event stream")

    def stream_events(self) -> Iterator[ProcessEvent]:
        for event in self._event_source():
            yield ProcessEvent(event)

    def disconnect(self) -> None:
        events = self._events
        if events is not None and hasattr(events, "close"):
            events.close()
        self._events = None

    def _handle_events(self) -> Iterator[Tuple[Optional[Stdout], Optional[Stderr], Optional[PtyOutput]]]:
        try:
            for event in self.stream_events():
                if event.data:
                    if event.data.stdout:
                        self._stdout += event.data.stdout
                        yield event.data.stdout, None, None
                    if event.data.stderr:
                        self._stderr += event.data.stderr
                        yield None, event.data.stderr, None
                    if event.data.pty:
                        self._stdout += event.data.pty
                        yield None, None, event.data.pty
                if event.end:
                    self._result = CommandResult({
                        "stdout": self._stdout,
                        "stderr": self._stderr,
                        "exit_code": int(event.end.get("exitCode", event.end.get("exit_code", UNKNOWN_EXIT_CODE))),
                        "error": event.end.get("error", ""),
                        "exited": event.end.get("exited"),
                        "process_status": event.end.get("status", ""),
                    })
        except Exception as exc:
            self._iteration_exception = exc
            raise

    def wait(
            self,
            on_pty: Optional[OutputHandler] = None,
            on_stdout: Optional[OutputHandler] = None,
            on_stderr: Optional[OutputHandler] = None,
    ) -> CommandResult:
        if self._result is not None:
            if self._iteration_exception is not None:
                raise self._iteration_exception
            if self._result.exit_code != 0:
                raise CommandExitException(
                    stdout=self._result.stdout,
                    stderr=self._result.stderr,
                    exit_code=self._result.exit_code,
                    error=self._result.error,
                )
            return self._result
        try:
            for stdout, stderr, pty in self:
                if stdout is not None and on_stdout:
                    on_stdout(stdout)
                elif stderr is not None and on_stderr:
                    on_stderr(stderr)
                elif pty is not None and on_pty:
                    on_pty(pty)
        except StopIteration:
            pass
        except Exception as exc:
            self._iteration_exception = exc
        if self._iteration_exception is not None:
            raise self._iteration_exception
        if self._result is None:
            raise SandboxError("Command ended without an end event")
        if self._result.exit_code != 0:
            raise CommandExitException(
                stdout=self._result.stdout,
                stderr=self._result.stderr,
                exit_code=self._result.exit_code,
                error=self._result.error,
            )
        return self._result

    def kill(self, signal: int = 15, request_timeout: Optional[float] = None) -> bool:
        return self._sandbox.client.send_signal(pid=self.pid, tag=self.tag, signal=signal, request_timeout=request_timeout)


class CommandManager:
    def __init__(self, sandbox: "Sandbox"):
        self._sandbox = sandbox

    def run(
            self,
            cmd: str,
            args: Optional[List[str]] = None,
            cwd: Optional[str] = None,
            env: Optional[Dict[str, str]] = None,
            content: Optional[str] = None,
            background: bool = False,
            tag: Optional[str] = None,
            pty: Optional[Dict[str, int]] = None,
            stdin: Optional[Union[str, bytes]] = None,
            timeout: Optional[float] = None,
            on_stdout: Optional[OutputHandler] = None,
            on_stderr: Optional[OutputHandler] = None,
            request_timeout: Optional[float] = None,
    ):
        if background and (on_stdout is not None or on_stderr is not None):
            raise InvalidArgumentError("callbacks are only supported by foreground run() or CommandHandle.wait()")
        if timeout is not None and timeout < 0:
            raise InvalidArgumentError("timeout must not be negative")
        timeout_ms = math.ceil(timeout * 1000) if timeout is not None else None

        if background:
            response = self._sandbox.client.start_process(
                cmd=cmd,
                args=args or [],
                cwd=cwd,
                env=env or {},
                content=content,
                background=True,
                tag=tag,
                pty=pty,
                stdin=stdin,
                timeout_ms=timeout_ms,
                request_timeout=request_timeout,
            )
            process = response.get("process")
            if not process:
                raise RuntimeError(response.get("error") or "failed to start background process")
            return CommandHandle(self._sandbox, process, events=response.get("events"))

        if on_stdout is not None or on_stderr is not None:
            events = self._sandbox.client.stream_process(
                cmd=cmd,
                args=args or [],
                cwd=cwd,
                env=env or {},
                content=content,
                background=False,
                tag=tag,
                pty=pty,
                stdin=stdin,
                timeout_ms=timeout_ms,
                request_timeout=request_timeout,
            )
            try:
                first_event = ProcessEvent(next(events))
            except StopIteration as exc:
                raise SandboxError("Failed to start process: missing start event") from exc
            if not first_event.start:
                raise SandboxError(f"Failed to start process: expected start event, got {first_event.raw}")
            process = {
                "pid": first_event.start.get("pid", 0),
                "tag": tag or "",
                "running": True,
                "startedAt": "",
                "exitCode": -1,
                "finishedAt": "",
                "stdout": "",
                "stderr": "",
                "config": {
                    "cmd": cmd,
                    "args": args or [],
                    "env": env or {},
                    "cwd": cwd,
                },
            }
            if pty is not None:
                process["config"]["pty"] = pty
            handle = CommandHandle(self._sandbox, process, events=events)
            return handle.wait(on_stdout=on_stdout, on_stderr=on_stderr)

        response = self._sandbox.client.start_process(
            cmd=cmd,
            args=args or [],
            cwd=cwd,
            env=env or {},
            content=content,
            background=False,
            tag=tag,
            pty=pty,
            stdin=stdin,
            timeout_ms=timeout_ms,
            request_timeout=request_timeout,
        )
        result = CommandResult(response)
        if result.exit_code != 0:
            raise CommandExitException(
                stdout=result.stdout,
                stderr=result.stderr,
                exit_code=result.exit_code,
                error=result.error,
            )
        return result

    def connect(self, pid: Optional[int] = None, tag: Optional[str] = None,
                request_timeout: Optional[float] = None) -> CommandHandle:
        if pid is None and not tag:
            raise InvalidArgumentError("process pid or tag is required")
        events = iter(self._sandbox.client.connect_process(pid=pid, tag=tag, request_timeout=request_timeout))
        try:
            start_event = ProcessEvent(next(events))
        except StopIteration as exc:
            raise SandboxError("Failed to connect to process: missing start event") from exc
        if not start_event.start:
            raise SandboxError(f"Failed to connect to process: expected start event, got {start_event.raw}")
        process = {
            "pid": start_event.start.get("pid", pid or 0),
            "tag": tag or "",
        }
        return CommandHandle(self._sandbox, process, events=events)

    def list(self, request_timeout: Optional[float] = None) -> List[ProcessInfo]:
        return [ProcessInfo.from_dict(process) for process in self._sandbox.client.list_processes(request_timeout=request_timeout)]

    def kill(self, pid: Optional[int] = None, tag: Optional[str] = None, signal: int = 15,
             request_timeout: Optional[float] = None) -> bool:
        return self._sandbox.client.send_signal(pid=pid, tag=tag, signal=signal, request_timeout=request_timeout)


class FilesManager:
    def __init__(self, sandbox: "Sandbox"):
        self._sandbox = sandbox

    def write(self, path: str, data: Union[str, bytes, IO], request_timeout: Optional[float] = None) -> WriteInfo:
        result = self.write_files([{"path": path, "data": data}], request_timeout=request_timeout)
        if len(result) != 1:
            raise RuntimeError("Received unexpected response from write operation")
        return result[0]

    def write_files(self, files: List[WriteEntry], request_timeout: Optional[float] = None) -> List[WriteInfo]:
        if not files:
            return []
        specs: List[Dict[str, Any]] = []
        for item in files:
            if not isinstance(item, dict):
                raise InvalidArgumentError(f"invalid file spec: {item}")
            if "path" not in item or "data" not in item:
                raise InvalidArgumentError("invalid file spec, need 'path' and 'data': " + str(item))
            data = item["data"]
            if not isinstance(data, (str, bytes, TextIOBase, IOBase)):
                raise InvalidArgumentError(f"unsupported data type for file {item['path']}: {type(data)}")
            specs.append({"filepath": item["path"], "content": data})
        return self._post_file_specs(specs, request_timeout=request_timeout)

    def upload(self, *args, request_timeout: Optional[float] = None, **kwargs):
        specs = self._normalize_upload_specs(*args, **kwargs)
        infos = self._post_file_specs(specs, request_timeout=request_timeout)
        return infos[0] if len(infos) == 1 else infos

    @overload
    def read(self, path: str, format: Literal["text"] = "text", request_timeout: Optional[float] = None) -> str:
        ...

    @overload
    def read(self, path: str, format: Literal["bytes"], request_timeout: Optional[float] = None) -> bytes:
        ...

    @overload
    def read(self, path: str, format: Literal["stream"], request_timeout: Optional[float] = None) -> Iterator[bytes]:
        ...

    def read(self, path: str, format: Literal["text", "bytes", "stream"] = "text", request_timeout: Optional[float] = None):
        if format not in {"text", "bytes", "stream"}:
            raise InvalidArgumentError("format must be one of: text, bytes, stream")
        if format == "stream":
            return self._sandbox.client.stream_file(path, request_timeout=request_timeout)
        content = self._sandbox.client.read_file(path, request_timeout=request_timeout)
        if format == "bytes":
            return content
        return content.decode("utf-8")

    def download(self, remote_path: str, local_path: str, request_timeout: Optional[float] = None) -> Dict[str, Any]:
        return self._sandbox.client.get_file(remote_path, local_path, request_timeout=request_timeout)

    def list(self, path: str, depth: int = 1, request_timeout: Optional[float] = None) -> List[EntryInfo]:
        return [EntryInfo.from_dict(item) for item in self._sandbox.client.list_files(path, depth=depth, request_timeout=request_timeout)]

    def search(self, path: str, pattern: str, exclude_patterns: Optional[List[str]] = None,
               request_timeout: Optional[float] = None) -> List[EntryInfo]:
        return [EntryInfo.from_dict(item) for item in self._sandbox.client.search_files(path, pattern, exclude_patterns=exclude_patterns, request_timeout=request_timeout)]

    @staticmethod
    def _normalize_upload_specs(*args, **kwargs) -> List[Dict[str, Any]]:
        if "files" in kwargs:
            if args:
                raise TypeError("cannot mix args and 'files' keyword")
            file_specs = kwargs["files"]
        elif len(args) == 2:
            local_path, remote_path = args
            file_specs = [{"local_path": local_path, "remote_path": remote_path}]
        elif len(args) == 1 and isinstance(args[0], (list, tuple)):
            file_specs = args[0]
        else:
            raise TypeError("usage: upload(local, remote) or upload([spec, ...]) or upload(files=[...])")

        files: List[Dict[str, Any]] = []
        for item in file_specs:
            if not isinstance(item, dict):
                raise InvalidArgumentError(f"invalid file spec: {item}")
            if "content" in item and "filepath" in item:
                content = item["content"]
                if not isinstance(content, (str, bytes, TextIOBase, IOBase)):
                    raise InvalidArgumentError(f"unsupported data type for file {item['filepath']}: {type(content)}")
                files.append({"filepath": item["filepath"], "content": content})
                continue
            if all(key in item for key in ("local_path", "remote_path")):
                local_path = item["local_path"]
                if not os.path.exists(local_path):
                    raise FileNotFoundError(f"local file not found: {local_path}")
                if not os.path.isfile(local_path):
                    raise FileNotFoundError(f"not a file: {local_path}")
                files.append({"filepath": item["remote_path"], "local_path": local_path})
                continue
            raise InvalidArgumentError(
                "invalid file spec, need 'content'+'filepath' or 'local_path'+'remote_path': "
                + str(item)
            )
        return files

    def _post_file_specs(self, files: List[Dict[str, Any]], request_timeout: Optional[float] = None) -> List[WriteInfo]:
        if not files:
            return []
        if request_timeout is None:
            result = self._sandbox.client.post_files(files)
        else:
            result = self._sandbox.client.post_files(files, request_timeout=request_timeout)
        if result.get("status") != self._sandbox.client.STATUS_SUCCESS:
            message = result.get("error") or result.get("message") or "file upload failed"
            raise RuntimeError(message)
        uploaded_count = int(result.get("uploaded_count", 0) or 0)
        if uploaded_count != len(files):
            raise RuntimeError(f"file upload incomplete: uploaded {uploaded_count}, expected {len(files)}")
        entries = result.get("entries") or []
        if len(entries) != len(files):
            raise RuntimeError(f"file upload response incomplete: got {len(entries)} entries, expected {len(files)}")
        return [WriteInfo.from_dict(entry) for entry in entries]


class Sandbox:
    def __init__(
            self,
            sandbox_id: Optional[str] = None,
            template_name: Optional[str] = None,
            template_id: Optional[str] = None,
            vcpu_num: Optional[int] = None,
            vcpu_max: Optional[int] = None,
            ram_mb: Optional[int] = None,
            volume_mounts: Optional[List[Dict[str, Any]]] = None,
            env: Optional[Dict[str, str]] = None,
            network: Optional[Dict[str, Any]] = None,
            vmm_name: Optional[str] = None,
    ):
        warn_onproxy()

        self.unix_socket = DEFAULT_UNIX_SOCKET
        self._session = requests_unixsocket.Session()

        self.sandbox_id = sandbox_id or generate_random_id()
        # Unset template and resource fields are filled from sandbox.default_spec.
        if (template_name and template_name.strip()) and (template_id and template_id.strip()):
            raise ValueError("template_name and template_id are mutually exclusive")
        self.template_name = template_name
        self.template_id = template_id

        self.ip = None
        self.agent_token = None
        self._client = None
        self.control_plane_only = False
        self.vcpu_num = vcpu_num
        self.vcpu_max = vcpu_max
        self.ram_mb = ram_mb
        # Empty means conchd picks its configured sandbox.backend.
        self.vmm_name = vmm_name
        self.image_name = None
        self.snapshot_id = None
        self.started_at = None
        self.end_at = None
        self.disk_size_mb = None
        self.conch_init_version = None
        self.alias = None
        self.commands = CommandManager(self)
        self.files = FilesManager(self)
        self.volume_mounts = volume_mounts or []
        self.env = env
        self.network = dict(network) if network is not None else None
        self.metadata: Dict[str, str] = {}
        self.lifecycle: Dict[str, Any] = {}

    @property
    def client(self):
        if self._client is None:
            if self.control_plane_only:
                raise RuntimeError("Agent credentials unavailable for retrieved sandbox")
            raise RuntimeError("Sandbox agent client is not initialized")
        return self._client

    @client.setter
    def client(self, value):
        self._client = value
        if value is not None:
            self.control_plane_only = False

    def _build_control_plane_url(self, path: str) -> str:
        return f"http+unix://{quote_plus(self.unix_socket)}{path}"

    def _post_control_plane_requests(self, path: str, payload: Dict[str, Any]) -> Dict[str, Any]:
        url = self._build_control_plane_url(path)
        response = self._session.post(url, json=payload)
        response.raise_for_status()
        return response.json()

    def _get_control_plane_response(self, path: str):
        response = self._session.get(self._build_control_plane_url(path))
        response.raise_for_status()
        return response

    def _get_control_plane_requests(
            self,
            path: str,
            params: Optional[Dict[str, Any]] = None,
    ) -> Any:
        response = self._session.get(self._build_control_plane_url(path), params=params)
        response.raise_for_status()
        return {} if response.status_code == HTTP_NO_CONTENT else response.json()

    def _delete_control_plane_requests(
            self,
            path: str,
            params: Optional[Dict[str, Any]] = None,
    ) -> Any:
        response = self._session.delete(self._build_control_plane_url(path), params=params)
        response.raise_for_status()
        return {} if response.status_code == HTTP_NO_CONTENT else response.json()

    def _put_control_plane_requests(self, path: str, payload: Dict[str, Any]) -> Any:
        response = self._session.put(self._build_control_plane_url(path), json=payload)
        response.raise_for_status()
        return {} if response.status_code == HTTP_NO_CONTENT else response.json()

    def _build_create_payload(self) -> Dict[str, Any]:
        # Resource fields are omitted when unset so conchd applies default_spec.
        payload: Dict[str, Any] = {
            SANDBOX_ID_KEY: self.sandbox_id,
        }
        if self.vcpu_num:
            payload["vcpu_num"] = self.vcpu_num
        if self.vcpu_max:
            payload["vcpu_max"] = self.vcpu_max
        if self.ram_mb:
            payload["ram_mb"] = self.ram_mb
        has_template_name = bool(self.template_name and self.template_name.strip())
        has_template_id = bool(self.template_id and self.template_id.strip())
        if has_template_name and has_template_id:
            raise ValueError("template_name and template_id are mutually exclusive")
        if has_template_name:
            payload[TEMPLATE_NAME_KEY] = self.template_name
        if has_template_id:
            payload[TEMPLATE_ID_KEY] = self.template_id
        # conchd matches the VMM name verbatim, so trim before sending.
        if self.vmm_name and self.vmm_name.strip():
            payload["vmm_name"] = self.vmm_name.strip()
        if self.volume_mounts:
            payload[VOLUME_MOUNTS_KEY] = self.volume_mounts
        if self.env is not None:
            payload["env"] = self.env
        if self.network is not None:
            payload["network"] = self.network
        return payload

    def _apply_sandbox_record(self, record: Dict[str, Any]) -> None:
        if not isinstance(record, dict):
            return
        self.sandbox_id = record.get("sandboxID") or self.sandbox_id
        self.template_name = record.get("templateName") or self.template_name
        self.template_id = record.get("templateID") or self.template_id
        self.ip = record.get("domain") or self.ip
        if record.get("conchInitAccessToken"):
            self.agent_token = record["conchInitAccessToken"]
        self.vcpu_num = record.get("cpuCount") or self.vcpu_num
        self.ram_mb = record.get("memoryMB") or self.ram_mb
        for attribute, field_name in {
            "image_name": "imageName",
            "snapshot_id": "snapshotID",
            "started_at": "startedAt",
            "end_at": "endAt",
            "disk_size_mb": "diskSizeMB",
            "conch_init_version": "conchInitVersion",
            "alias": "alias",
        }.items():
            if field_name in record:
                setattr(self, attribute, record[field_name])
        if "metadata" in record:
            self.metadata = dict(record["metadata"] or {})
        if "lifecycle" in record:
            self.lifecycle = dict(record["lifecycle"] or {})
        if "volumeMounts" in record:
            self.volume_mounts = list(record["volumeMounts"] or [])
        if "network" in record:
            self.network = dict(record["network"] or {})
        if self.ip and self.agent_token:
            if self._client:
                try:
                    self._client.close()
                except Exception:
                    pass
            self._client = AgentClient(host=self.ip, token=self.agent_token)

    def _do_create(self):
        payload = self._build_create_payload()

        try:
            result = self._post_control_plane_requests(SANDBOX_CREATE_PATH, payload)
            self._apply_sandbox_record(result)
            if not self.ip or not self.agent_token:
                raise RuntimeError("Sandbox creation failed: incomplete response")
            return self

        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    def delete(self, sandbox_id: Optional[str] = None) -> bool:
        target_id = sandbox_id if sandbox_id else self.sandbox_id
        if not target_id:
            raise RuntimeError("Cannot delete sandbox without sandbox_id")
        path = SANDBOX_DELETE_PATH_TEMPLATE.format(
            sandbox_id=quote(str(target_id), safe="")
        )

        try:
            self._delete_control_plane_requests(path)
            if target_id == self.sandbox_id:
                self._clear_sandbox_state()
            return True

        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    def _clear_sandbox_state(self) -> None:
        client = self._client
        self._client = None
        self.control_plane_only = False
        self.sandbox_id = ""
        self.ip = None
        self.agent_token = None
        self.template_name = None
        self.template_id = None
        self.vcpu_num = None
        self.vcpu_max = None
        self.ram_mb = None
        self.vmm_name = None
        self.image_name = None
        self.snapshot_id = None
        self.started_at = None
        self.end_at = None
        self.disk_size_mb = None
        self.conch_init_version = None
        self.alias = None
        self.volume_mounts = []
        self.env = None
        self.network = None
        self.metadata = {}
        self.lifecycle = {}
        if client is not None:
            try:
                client.close()
            except Exception:
                # Local connection cleanup must not undo a successful delete.
                pass

    @classmethod
    def service_health(cls) -> bool:
        try:
            sbx = cls()
            return sbx._get_control_plane_response(SERVICE_HEALTH_PATH).status_code == HTTP_NO_CONTENT
        except requests.exceptions.RequestException:
            return False

    @classmethod
    def list(
            cls,
            state: Optional[List[str]] = None,
            limit: Optional[int] = None,
    ) -> List[Dict[str, Any]]:
        sbx = cls()
        params: Dict[str, Any] = {}
        if state:
            params["state"] = state
        if limit is not None:
            params["limit"] = limit
        try:
            result = sbx._get_control_plane_requests(SANDBOX_LIST_PATH, params or None)
        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))
        return result if isinstance(result, list) else []

    @classmethod
    def get(
            cls,
            sandbox_id: str,
    ) -> "Sandbox":
        if not sandbox_id:
            raise RuntimeError("Cannot get sandbox without sandbox_id")
        sbx = cls(sandbox_id=sandbox_id)
        path = SANDBOX_GET_PATH_TEMPLATE.format(
            sandbox_id=quote(str(sandbox_id), safe="")
        )
        try:
            sbx._apply_sandbox_record(sbx._get_control_plane_requests(path))
        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))
        sbx.control_plane_only = sbx._client is None
        return sbx

    @staticmethod
    def delete_sandbox(
            sandbox_id: str,
    ):
        sbx = Sandbox(sandbox_id=sandbox_id)
        return sbx.delete(sandbox_id=sandbox_id)

    def checkpoint(self, template_name: str):
        if not template_name or not template_name.strip():
            raise ValueError("template_name is required")
        template_name = template_name.strip()
        payload = {
            SANDBOX_ID_KEY: self.sandbox_id,
            TEMPLATE_NAME_KEY: template_name,
        }

        try:
            result = self._post_control_plane_requests(SANDBOX_CHECKPOINT_PATH, payload)
            result[SANDBOX_ID_KEY] = self.sandbox_id
            template_id = result.get(TEMPLATE_ID_RESP_KEY)
            return TemplateInfo(
                template_name=template_name,
                template_id=template_id,
                sandbox_id=self.sandbox_id
            )

        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    def suspend(self) -> bool:
        return self._lifecycle(SANDBOX_SUSPEND_PATH)

    def resume(self) -> bool:
        return self._lifecycle(SANDBOX_RESUME_PATH)

    def update_network(
            self,
            allow_out: Optional[List[str]] = None,
            deny_out: Optional[List[str]] = None,
            allow_in: Optional[List[str]] = None,
            deny_in: Optional[List[str]] = None,
            allow_internet_access: Optional[bool] = None,
    ) -> bool:
        payload: Dict[str, Any] = {}
        for value, key in (
                (allow_out, "allowOut"),
                (deny_out, "denyOut"),
                (allow_in, "allowIn"),
                (deny_in, "denyIn"),
        ):
            if value is not None:
                payload[key] = list(value)
        if allow_internet_access is not None:
            payload["allow_internet_access"] = allow_internet_access

        path = SANDBOX_NETWORK_PATH_TEMPLATE.format(
            sandbox_id=quote(str(self.sandbox_id), safe="")
        )
        try:
            self._put_control_plane_requests(path, payload)
            self.network = payload
            return True
        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    def _lifecycle(self, path: str) -> bool:
        payload = {
            SANDBOX_ID_KEY: self.sandbox_id,
        }
        try:
            self._post_control_plane_requests(path, payload)
            return True
        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    @classmethod
    def create(
            cls,
            template_name: Optional[str] = None,
            template_id: Optional[str] = None,
            sandbox_id: Optional[str] = None,
            vcpu_num: Optional[int] = None,
            vcpu_max: Optional[int] = None,
            ram_mb: Optional[int] = None,
            volume_mounts: Optional[List[Dict[str, Any]]] = None,
            env: Optional[Dict[str, str]] = None,
            network: Optional[Dict[str, Any]] = None,
            vmm_name: Optional[str] = None,
    ) -> "Sandbox":
        sbx = cls(
            sandbox_id=sandbox_id,
            template_name=template_name,
            template_id=template_id,
            vcpu_num=vcpu_num,
            vcpu_max=vcpu_max,
            ram_mb=ram_mb,
            volume_mounts=volume_mounts,
            env=env,
            network=network,
            vmm_name=vmm_name,
        )
        return sbx._do_create()

    def get_info(self) -> SandboxInfo:
        return SandboxInfo(
            sandbox_id=self.sandbox_id,
            ip=self.ip if self.ip else "",
            template_name=self.template_name,
            template_id=self.template_id,
        )

    def health_check(self, request_timeout: Optional[float] = None) -> Dict[str, Any]:
        # Check sandbox health status
        client = self.client
        try:
            return client.health_check(request_timeout=request_timeout)
        except Exception as e:
            return {
                STATUS_KEY: "ERROR",
                MESSAGE_KEY: f"Health check failed: {e}"
            }

    def __enter__(self) -> 'Sandbox':
        # Context manager entry (return self)
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        # Context manager exit (auto-delete sandbox)
        try:
            self.delete()
        except Exception as e:
            print(f"Warning: Failed to delete sandbox during exit: {e}")
