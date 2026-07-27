import os
from enum import Enum
from io import IOBase, TextIOBase
from typing import IO, Callable, Iterator, Optional, Dict, Any, List, Tuple, Literal, overload, TypedDict, Union
from dataclasses import dataclass, field
from urllib.parse import quote_plus
import requests
import requests_unixsocket
import uuid
import secrets

# Try relative imports first (when imported as a package), fall back to absolute imports
from .client import AgentClient
from .config_loader import load_config
from .errors import InvalidArgumentError, NotFoundError, SandboxError


# API keys
SANDBOX_ID_KEY = "sandbox_id"
NAMESPACE_KEY = "namespace"
TEMPLATE_ID_KEY = "template_id"
VMM_NAME_KEY = "vmm_name"
VCPU_NUM_KEY = "vcpu_num"
VCPU_MAX_KEY = "vcpu_max"
RAM_MB_KEY = "ram_mb"
STATUS_KEY = "status"
ERROR_KEY = "error"
MESSAGE_KEY = "message"
IP_KEY = "ip"
AGENT_TOKEN_KEY = "agent_token"
TEMPLATE_ID_RESP_KEY = "template_id"

# Config keys
CFG_SANDBOX_SECTION = "sandbox"
CFG_IMAGE_SECTION = "image"
CFG_UNIX_SOCKET_KEY = "unix_socket"
CFG_API_URL_KEY = "api_url"

# API paths
SANDBOX_CREATE_PATH = "/api/sandbox/create"
SANDBOX_DELETE_PATH = "/api/sandbox/delete"
SANDBOX_SUSPEND_PATH = "/api/sandbox/suspend"
SANDBOX_RESUME_PATH = "/api/sandbox/resume"
SANDBOX_STOP_PATH = "/api/sandbox/stop"
SANDBOX_CHECKPOINT_PATH = "/api/sandbox/checkpoint"

RANDOM_ID_HEX_BYTES = 12
UNKNOWN_EXIT_CODE = -1

def generate_random_id(prefix: str = "sandbox_") -> str:
    return prefix + secrets.token_hex(RANDOM_ID_HEX_BYTES)


def _request_exception_message(exc: requests.exceptions.RequestException) -> str:
    response = getattr(exc, "response", None)
    if response is not None and getattr(response, "text", None):
        return response.text
    return str(exc)

@dataclass
class TemplateInfo:
    template_id: str
    sandbox_id: str


@dataclass
class SandboxInfo:
    # TODO: Extend this with more sandbox metadata once the SDK surface is finalized.
    sandbox_id: str
    ip: str
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

    def kill(self, signal: int = 15) -> bool:
        return self._sandbox.client.send_signal(pid=self.pid, tag=self.tag, signal=signal)


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
            on_stdout: Optional[OutputHandler] = None,
            on_stderr: Optional[OutputHandler] = None,
    ):
        if background and (on_stdout is not None or on_stderr is not None):
            raise InvalidArgumentError("callbacks are only supported by foreground run() or CommandHandle.wait()")

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

    def connect(self, pid: Optional[int] = None, tag: Optional[str] = None) -> CommandHandle:
        if pid is None and not tag:
            raise InvalidArgumentError("process pid or tag is required")
        events = iter(self._sandbox.client.connect_process(pid=pid, tag=tag))
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

    def list(self) -> List[ProcessInfo]:
        return [ProcessInfo.from_dict(process) for process in self._sandbox.client.list_processes()]

    def kill(self, pid: Optional[int] = None, tag: Optional[str] = None, signal: int = 15) -> bool:
        return self._sandbox.client.send_signal(pid=pid, tag=tag, signal=signal)


class FilesManager:
    def __init__(self, sandbox: "Sandbox"):
        self._sandbox = sandbox

    def write(self, path: str, data: Union[str, bytes, IO]) -> WriteInfo:
        result = self.write_files([{"path": path, "data": data}])
        if len(result) != 1:
            raise RuntimeError("Received unexpected response from write operation")
        return result[0]

    def write_files(self, files: List[WriteEntry]) -> List[WriteInfo]:
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
        return self._post_file_specs(specs)

    def upload(self, *args, **kwargs):
        specs = self._normalize_upload_specs(*args, **kwargs)
        infos = self._post_file_specs(specs)
        return infos[0] if len(infos) == 1 else infos

    @overload
    def read(self, path: str, format: Literal["text"] = "text") -> str:
        ...

    @overload
    def read(self, path: str, format: Literal["bytes"]) -> bytes:
        ...

    @overload
    def read(self, path: str, format: Literal["stream"]) -> Iterator[bytes]:
        ...

    def read(self, path: str, format: Literal["text", "bytes", "stream"] = "text"):
        if format not in {"text", "bytes", "stream"}:
            raise InvalidArgumentError("format must be one of: text, bytes, stream")
        if format == "stream":
            return self._sandbox.client.stream_file(path)
        content = self._sandbox.client.read_file(path)
        if format == "bytes":
            return content
        return content.decode("utf-8")

    def download(self, remote_path: str, local_path: str) -> Dict[str, Any]:
        return self._sandbox.client.get_file(remote_path, local_path)

    def list(self, path: str, depth: int = 1) -> List[EntryInfo]:
        return [EntryInfo.from_dict(item) for item in self._sandbox.client.list_files(path, depth=depth)]

    def search(self, path: str, pattern: str, exclude_patterns: Optional[List[str]] = None) -> List[EntryInfo]:
        return [EntryInfo.from_dict(item) for item in self._sandbox.client.search_files(path, pattern, exclude_patterns=exclude_patterns)]

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

    def _post_file_specs(self, files: List[Dict[str, Any]]) -> List[WriteInfo]:
        if not files:
            return []
        result = self._sandbox.client.post_files(files)
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
            unix_socket: Optional[str] = None,
            api_url: Optional[str] = None,
            sandbox_id: Optional[str] = None,
            template_id: Optional[str] = None,
            namespace: Optional[str] = None,
            vcpu_num: Optional[int] = None,
            vcpu_max: Optional[int] = None,
            ram_mb: Optional[int] = None,
            config_path: Optional[str] = None,
    ):
        self._config: Dict[str, Any] = load_config(config_path=config_path)
        sandbox_cfg = self._config[CFG_SANDBOX_SECTION]

        configured_unix_socket = sandbox_cfg.get(CFG_UNIX_SOCKET_KEY, "")
        configured_api_url = sandbox_cfg.get(CFG_API_URL_KEY, "")
        self.unix_socket = unix_socket if unix_socket is not None else configured_unix_socket
        self.api_url = api_url.rstrip('/') if api_url else configured_api_url.rstrip('/')
        self._session = requests_unixsocket.Session() if self.unix_socket else requests.Session()

        config_sandbox_id = sandbox_cfg.get(SANDBOX_ID_KEY, "")
        self.sandbox_id = sandbox_id or config_sandbox_id or generate_random_id()
        self.namespace = namespace or ""
        self.template_id = template_id or sandbox_cfg.get(TEMPLATE_ID_KEY, "")

        self.ip = None
        self.agent_token = None
        self.client = None
        self.vcpu_num = vcpu_num
        self.vcpu_max = vcpu_max
        self.ram_mb = ram_mb
        self.commands = CommandManager(self)
        self.files = FilesManager(self)


    def _build_control_plane_url(self, path: str) -> str:
        if self.unix_socket:
            encoded_socket = quote_plus(self.unix_socket)
            return f"http+unix://{encoded_socket}{path}"
        if self.api_url:
            return f"{self.api_url}{path}"
        raise ValueError("Either sandbox.unix_socket or sandbox.api_url must be configured")

    def _post_control_plane_requests(self, path: str, payload: Dict[str, Any]) -> Dict[str, Any]:
        url = self._build_control_plane_url(path)
        response = self._session.post(url, json=payload)
        response.raise_for_status()
        return response.json()

    def _get_vmm_name(self) -> str:
        return self._config[CFG_IMAGE_SECTION].get(VMM_NAME_KEY, "")

    def _build_create_payload(self) -> Dict[str, Any]:
        if not self.template_id:
            raise ValueError("template_id is required")

        config = self._config[CFG_IMAGE_SECTION]
        return {
            NAMESPACE_KEY: self.namespace,
            TEMPLATE_ID_KEY: self.template_id,
            VMM_NAME_KEY: self._get_vmm_name(),
            SANDBOX_ID_KEY: self.sandbox_id,
            VCPU_NUM_KEY: self.vcpu_num or config[VCPU_NUM_KEY],
            VCPU_MAX_KEY: self.vcpu_max or config[VCPU_MAX_KEY],
            RAM_MB_KEY: self.ram_mb or config[RAM_MB_KEY],
        }

    def _update_client_from_result(self, result: Dict[str, Any]):
        # Initialize/update the AgentClient based on sandbox creation result
        status = result.get(STATUS_KEY)
        server_ip = result.get(IP_KEY)
        agent_token = result.get(AGENT_TOKEN_KEY)

        if status == "ok" and server_ip:
            if not agent_token:
                raise RuntimeError("Sandbox creation failed: missing agent_token in response")
            self.ip = server_ip
            self.agent_token = agent_token
            if self.client:
                try:
                    self.client.close()
                except Exception:
                    pass
            self.client = AgentClient(host=self.ip, token=self.agent_token)
        else:
            error_val = result.get(ERROR_KEY)
            error_msg = str(error_val) if error_val is not None else "Unknown error"
            raise RuntimeError(f"Sandbox creation failed: {error_msg}")

    def _do_create(self):
        payload = self._build_create_payload()

        try:
            result = self._post_control_plane_requests(SANDBOX_CREATE_PATH, payload)
            result[SANDBOX_ID_KEY] = self.sandbox_id
            self._update_client_from_result(result)
            return self

        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    def delete(self, sandbox_id: Optional[str] = None) -> bool:
        target_id = sandbox_id if sandbox_id else self.sandbox_id
        payload = {
            NAMESPACE_KEY: self.namespace,
            SANDBOX_ID_KEY: target_id,
        }

        try:
            result = self._post_control_plane_requests(SANDBOX_DELETE_PATH, payload)
            result[SANDBOX_ID_KEY] = target_id
            return True

        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    @staticmethod
    def delete_sandbox(
            sandbox_id: str,
            unix_socket: Optional[str] = None,
            api_url: Optional[str] = None,
            namespace: Optional[str] = None,
            config_path: Optional[str] = None,
    ):
        sbx = Sandbox(
            sandbox_id=sandbox_id,
            unix_socket=unix_socket,
            api_url=api_url,
            namespace=namespace,
            config_path=config_path,
        )
        return sbx.delete(sandbox_id=sandbox_id)

    def checkpoint(self):
        payload = {
            NAMESPACE_KEY: self.namespace,
            SANDBOX_ID_KEY: self.sandbox_id,
        }

        try:
            result = self._post_control_plane_requests(SANDBOX_CHECKPOINT_PATH, payload)
            result[SANDBOX_ID_KEY] = self.sandbox_id
            template_id = result.get(TEMPLATE_ID_RESP_KEY)
            return TemplateInfo(
                template_id=template_id,
                sandbox_id=self.sandbox_id
            )

        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    def suspend(self) -> bool:
        return self._lifecycle(SANDBOX_SUSPEND_PATH)

    def resume(self) -> bool:
        return self._lifecycle(SANDBOX_RESUME_PATH)

    def stop(self) -> bool:
        return self._lifecycle(SANDBOX_STOP_PATH)

    def _lifecycle(self, path: str) -> bool:
        payload = {
            NAMESPACE_KEY: self.namespace,
            SANDBOX_ID_KEY: self.sandbox_id,
        }
        try:
            self._post_control_plane_requests(path, payload)
            return True
        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    @classmethod
    def create(cls, template_id: Optional[str] = None, **kwargs) -> "Sandbox":
        sbx = cls(template_id=template_id, **kwargs)
        return sbx._do_create()
    
    def get_info(self) -> SandboxInfo:
        return SandboxInfo(
            sandbox_id=self.sandbox_id,
            ip=self.ip if self.ip else "",
            template_id=self.template_id,
        )
    
    def health_check(self) -> Dict[str, Any]:
        # Check sandbox health status
        try:
            return self.client.health_check()
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
