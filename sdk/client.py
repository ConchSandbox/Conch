import os
import sys
import tempfile
from io import IOBase, TextIOBase
from typing import Any, Dict, Iterator, List, Optional

import requests
from connectrpc.code import Code
from connectrpc.compat import google_protobuf_binary_codec
from connectrpc.errors import ConnectError

PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from api.py_proto import agent_connect
from api.py_proto import agent_pb2
from .errors import InvalidArgumentError, handle_rpc_error


class AgentClient:
    DEFAULT_AGENT_PORT = 4064
    STATUS_SUCCESS = 0
    STATUS_FAILED = -1
    FILE_CHUNK_SIZE = 1024 * 1024

    def __init__(self, host: str, port: int = DEFAULT_AGENT_PORT, token: Optional[str] = None):
        self.host = host
        self.port = port
        self.token = token
        self.address = f"{host}:{port}"
        self.base_url = self._build_base_url(host, port)
        self.session = requests.Session()
        codec = google_protobuf_binary_codec()
        self.process_client = agent_connect.ProcessServiceClientSync(self.base_url, codec=codec)
        self.file_client = agent_connect.FileServiceClientSync(self.base_url, codec=codec)

    @staticmethod
    def _build_base_url(host: str, port: int) -> str:
        if host.startswith("http://") or host.startswith("https://"):
            return host.rstrip("/")
        return f"http://{host}:{port}"

    def _headers(self) -> Dict[str, str]:
        if not self.token:
            return {}
        return {"conch-init-token": self.token}

    def _url(self, path: str) -> str:
        return f"{self.base_url}{path}"

    def health_check(self) -> Dict[str, Any]:
        response = self.session.get(self._url("/health"))
        if response.status_code >= 400:
            raise RuntimeError(self._http_error(response))
        return response.json() if response.content else {"status": "OK", "message": "OK"}

    def start_process(
        self,
        cmd: str,
        cwd: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
        content: Optional[str] = None,
        args: Optional[list] = None,
        background: bool = False,
        tag: Optional[str] = None,
        pty: Optional[Dict[str, int]] = None,
    ) -> Dict[str, Any]:
        if content is not None and args:
            raise InvalidArgumentError("content cannot be used with args; write a file first and execute it via args")
        request = agent_pb2.StartProcessRequest(
            cmd=cmd,
            args=args or [],
            env=env or {},
            cwd=cwd or "",
            content=content or "",
            background=background,
            tag=tag or "",
        )
        if pty is not None:
            request.pty.CopyFrom(agent_pb2.PTY(cols=pty.get("cols", 0), rows=pty.get("rows", 0)))

        raw_events = self._rpc_call(self.process_client.start_process, request)
        events = self._rpc_iter(raw_events)
        if background:
            return self._start_background_process_response(request, events)
        return self._aggregate_process_response(events)

    def connect_process(self, process: Optional[Dict[str, Any]] = None, *, pid: Optional[int] = None,
        tag: Optional[str] = None) -> Iterator[Dict[str, Any]]:
        selector = self._process_selector(process, pid=pid, tag=tag)
        request = agent_pb2.ConnectProcessRequest(process=selector)
        raw_events = self._rpc_call(self.process_client.connect, request)
        for event in self._rpc_iter(raw_events):
            yield self._process_event_to_dict(event)

    def list_processes(self) -> List[Dict[str, Any]]:
        response = self._rpc_call(self.process_client.list, agent_pb2.ListProcessesRequest())
        return [self._process_info_to_dict(process) for process in response.processes]

    def send_signal(self, process: Optional[Dict[str, Any]] = None, *, pid: Optional[int] = None,
                    tag: Optional[str] = None, signal: int = 15) -> bool:
        selector = self._process_selector(process, pid=pid, tag=tag)
        request = agent_pb2.SendSignalRequest(process=selector, signal=signal)
        try:
            self.process_client.send_signal(request, headers=self._headers())
        except ConnectError as exc:
            if exc.code == Code.NOT_FOUND:
                return False
            raise handle_rpc_error(exc) from None
        return True

    def post_files(self, files: List[Dict[str, Any]]) -> Dict[str, Any]:
        uploaded_count = 0
        entries = []
        for file_spec in files:
            response = self._rpc_call(
                self.file_client.post_file_stream,
                self._iter_file_chunk_messages(file_spec),
            )
            uploaded_count += response.uploaded_count
            entries.extend(self._write_info_to_dict(entry) for entry in getattr(response, "entries", []))
        return {
            "status": self.STATUS_SUCCESS,
            "uploaded_count": uploaded_count,
            "entries": entries,
            "message": f"uploaded {uploaded_count} files",
        }

    def get_file(self, remote_path: str, local_path: str) -> Dict[str, Any]:
        local_dir = os.path.dirname(local_path) or "."
        os.makedirs(local_dir, exist_ok=True)
        size = 0
        request = agent_pb2.GetFileRequest(filepath=remote_path)
        chunks = self._rpc_call(self.file_client.get_file_stream, request)
        temp_path = None
        committed = False
        try:
            fd, temp_path = tempfile.mkstemp(prefix=".conch-download-", dir=local_dir)
            with os.fdopen(fd, "wb") as out:
                for chunk in self._rpc_iter(chunks):
                    out.write(chunk.content)
                    size += len(chunk.content)
            os.replace(temp_path, local_path)
            committed = True
        finally:
            if temp_path and not committed:
                try:
                    os.unlink(temp_path)
                except FileNotFoundError:
                    pass
        return {"status": self.STATUS_SUCCESS, "size": size, "message": "OK"}

    def stream_file(self, remote_path: str) -> Iterator[bytes]:
        request = agent_pb2.GetFileRequest(filepath=remote_path)
        chunks = self._rpc_call(self.file_client.get_file_stream, request)
        for chunk in self._rpc_iter(chunks):
            yield chunk.content

    def read_file(self, remote_path: str) -> bytes:
        return b"".join(self.stream_file(remote_path))

    def get_files(self, mappings: List[Dict[str, str]]) -> Dict[str, Any]:
        downloaded = 0
        failed = []
        for item in mappings:
            if "remote" not in item or "local" not in item:
                raise InvalidArgumentError("invalid file mapping, need 'remote' and 'local': " + str(item))
            try:
                self.get_file(item["remote"], item["local"])
                downloaded += 1
            except OSError as exc:
                failed.append({"remote": item["remote"], "local": item["local"], "error": str(exc)})
        status = self.STATUS_SUCCESS if not failed else self.STATUS_FAILED
        return {
            "status": status,
            "downloaded_count": downloaded,
            "failed": failed,
            "message": f"Downloaded {downloaded}, failed {len(failed)}",
        }

    def list_files(self, path: str, depth: int = 1) -> List[Dict[str, Any]]:
        request = agent_pb2.ListFilesRequest(path=path, depth=depth)
        response = self._rpc_call(self.file_client.list_files, request)
        return [self._file_entry_to_dict(entry) for entry in response.entries]

    def search_files(self, path: str, pattern: str, exclude_patterns: Optional[List[str]] = None) -> List[Dict[str, Any]]:
        request = agent_pb2.SearchFilesRequest(path=path, pattern=pattern)
        request.exclude_patterns.extend(exclude_patterns or [])
        response = self._rpc_call(self.file_client.search_files, request)
        return [self._file_entry_to_dict(entry) for entry in response.entries]

    def close(self):
        self.session.close()
        self.process_client.close()
        self.file_client.close()

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        self.close()

    @staticmethod
    def _http_error(response: requests.Response) -> str:
        body_text = response.text.strip()
        return f"HTTP {response.status_code}: {body_text}" if body_text else f"HTTP {response.status_code}"

    def _rpc_call(self, method, request):
        try:
            return method(request, headers=self._headers())
        except ConnectError as exc:
            raise handle_rpc_error(exc) from None

    @staticmethod
    def _rpc_iter(events: Iterator[Any]) -> Iterator[Any]:
        try:
            for event in events:
                yield event
        except ConnectError as exc:
            raise handle_rpc_error(exc) from None

    @staticmethod
    def _process_selector(process: Optional[Dict[str, Any]], *, pid: Optional[int],
                          tag: Optional[str]) -> agent_pb2.ProcessSelector:
        if process:
            return agent_pb2.ProcessSelector(pid=process.get("pid", 0), tag=process.get("tag", ""))
        if pid is not None:
            return agent_pb2.ProcessSelector(pid=pid)
        if tag:
            return agent_pb2.ProcessSelector(tag=tag)
        raise InvalidArgumentError("process pid or tag is required")

    def _start_background_process_response(
        self,
        request: agent_pb2.StartProcessRequest,
        events: Iterator[agent_pb2.ProcessEvent],
    ) -> Dict[str, Any]:
        try:
            first_event = next(events)
        except StopIteration:
            return {
                "status": self.STATUS_FAILED,
                "message": "failed to start background process: missing start event",
                "stdout": "",
                "stderr": "",
                "exit_code": -1,
                "error": "failed to start background process: missing start event",
                "process": None,
            }

        event = self._process_event_to_dict(first_event)
        if "start" not in event:
            response = self._aggregate_process_response(iter([first_event]))
            response["process"] = None
            return response

        process = {
            "pid": event["start"].get("pid", 0),
            "tag": request.tag,
            "running": True,
            "startedAt": "",
            "exitCode": -1,
            "finishedAt": "",
            "stdout": "",
            "stderr": "",
            "config": self._start_request_config_to_dict(request),
        }
        return {
            "status": self.STATUS_SUCCESS,
            "message": "OK",
            "stdout": "",
            "stderr": "",
            "exit_code": -1,
            "error": "",
            "process": process,
            "events": (self._process_event_to_dict(event) for event in events),
        }

    def _aggregate_process_response(self, events: Iterator[agent_pb2.ProcessEvent]) -> Dict[str, Any]:
        stdout = ""
        stderr = ""
        exit_code = -1
        error = "process ended without an end event"
        for raw_event in events:
            event = self._process_event_to_dict(raw_event)
            data = event.get("data") or {}
            stdout += data.get("stdout", "")
            # Preserve the previous unary API behavior where PTY output was returned as stdout.
            stdout += data.get("pty", "")
            stderr += data.get("stderr", "")
            end = event.get("end")
            if end is not None:
                exit_code = int(end.get("exitCode", -1))
                error = end.get("error", "")
                break

        status = self.STATUS_SUCCESS if exit_code == 0 and not error else self.STATUS_FAILED
        return {
            "status": status,
            "message": error or "OK",
            "stdout": stdout,
            "stderr": stderr,
            "exit_code": exit_code,
            "error": error,
            "process": None,
        }

    @classmethod
    def _start_request_config_to_dict(cls, request: agent_pb2.StartProcessRequest) -> Dict[str, Any]:
        data: Dict[str, Any] = {
            "cmd": request.cmd,
            "args": list(request.args),
            "env": dict(request.env),
            "cwd": request.cwd,
        }
        if request.HasField("pty"):
            data["pty"] = cls._pty_to_dict(request.pty)
        return data

    def _iter_file_chunk_messages(self, file_spec: Dict[str, Any]) -> Iterator[agent_pb2.FileChunk]:
        filepath = file_spec["filepath"]
        first = True
        if "local_path" in file_spec:
            with open(file_spec["local_path"], "rb") as src:
                while True:
                    content = src.read(self.FILE_CHUNK_SIZE)
                    if not content:
                        break
                    yield agent_pb2.FileChunk(filepath=filepath if first else "", content=content)
                    first = False
            if first:
                yield agent_pb2.FileChunk(filepath=filepath, content=b"")
            return

        content = file_spec["content"]
        if isinstance(content, str):
            content = content.encode()
        elif isinstance(content, TextIOBase):
            content = content.read().encode()
        if isinstance(content, IOBase):
            while True:
                chunk = content.read(self.FILE_CHUNK_SIZE)
                if not chunk:
                    break
                if isinstance(chunk, str):
                    chunk = chunk.encode()
                yield agent_pb2.FileChunk(filepath=filepath if first else "", content=chunk)
                first = False
        else:
            for offset in range(0, len(content), self.FILE_CHUNK_SIZE):
                yield agent_pb2.FileChunk(
                    filepath=filepath if first else "",
                    content=content[offset:offset + self.FILE_CHUNK_SIZE],
                )
                first = False
        if first:
            yield agent_pb2.FileChunk(filepath=filepath, content=b"")

    @classmethod
    def _process_info_to_dict(cls, info: agent_pb2.ProcessInfo) -> Dict[str, Any]:
        data: Dict[str, Any] = {
            "pid": info.pid,
            "tag": info.tag,
            "running": info.running,
            "startedAt": info.started_at,
            "exitCode": info.exit_code,
            "finishedAt": info.finished_at,
            "stdout": info.stdout,
            "stderr": info.stderr,
        }
        if info.HasField("config"):
            data["config"] = cls._process_config_to_dict(info.config)
        return data

    @staticmethod
    def _process_config_to_dict(config: agent_pb2.ProcessConfig) -> Dict[str, Any]:
        data: Dict[str, Any] = {
            "cmd": config.cmd,
            "args": list(config.args),
            "env": dict(config.env),
            "cwd": config.cwd,
        }
        if config.HasField("pty"):
            data["pty"] = AgentClient._pty_to_dict(config.pty)
        return data

    @staticmethod
    def _pty_to_dict(pty: agent_pb2.PTY) -> Dict[str, int]:
        return {"cols": pty.cols, "rows": pty.rows}

    @staticmethod
    def _process_event_to_dict(event: agent_pb2.ProcessEvent) -> Dict[str, Any]:
        which = event.WhichOneof("event")
        if which == "start":
            return {"start": {"pid": event.start.pid}}
        if which == "data":
            output = event.data.WhichOneof("output")
            if output == "stdout":
                return {"data": {"stdout": event.data.stdout}}
            if output == "stderr":
                return {"data": {"stderr": event.data.stderr}}
            if output == "pty":
                return {"data": {"pty": event.data.pty}}
            return {"data": {}}
        if which == "end":
            return {
                "end": {
                    "exitCode": event.end.exit_code,
                    "exited": event.end.exited,
                    "status": event.end.status,
                    "error": event.end.error,
                }
            }
        if which == "keepalive":
            return {"keepalive": {}}
        return {}

    @staticmethod
    def _write_info_to_dict(entry: agent_pb2.WriteInfo) -> Dict[str, Any]:
        return {
            "name": entry.name,
            "path": entry.path,
            "type": entry.type,
        }

    @staticmethod
    def _file_entry_to_dict(entry: agent_pb2.FileEntry) -> Dict[str, Any]:
        return {
            "name": entry.name,
            "path": entry.path,
            "type": entry.type,
            "size": entry.size,
            "permissions": entry.permissions,
            "modifiedTime": entry.modified_time,
            "metadata": dict(entry.metadata),
            "isDirectory": entry.is_directory,
        }
