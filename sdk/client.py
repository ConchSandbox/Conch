import codecs
import os
import secrets
import stat
import sys
from contextlib import contextmanager
from itertools import chain
from io import IOBase, TextIOBase
from typing import Any, Dict, Iterator, List, Optional, Union

import requests
from connectrpc.compat import google_protobuf_binary_codec
from connectrpc.errors import ConnectError
from pyqwest import SyncClient, SyncHTTPTransport

PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from api.py_proto import agent_connect
from api.py_proto import agent_pb2
from .errors import InvalidArgumentError, NotFoundError, handle_rpc_error


_DEFAULT_AGENT_REQUEST_TIMEOUT = 60.0


def _create_download_temp(local_dir: str) -> tuple[int, str]:
    for _ in range(100):
        temp_path = os.path.join(local_dir, f".conch-download-{secrets.token_hex(16)}")
        try:
            # os.open applies the caller's umask to 0o666, matching open(..., "wb").
            return os.open(temp_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o666), temp_path
        except FileExistsError:
            continue
    raise FileExistsError("failed to create a unique download temporary file")


def _resolve_request_timeout(request_timeout: Optional[float]) -> Optional[float]:
    if request_timeout == 0:
        return None
    if request_timeout is not None:
        if request_timeout < 0:
            raise InvalidArgumentError("request_timeout must not be negative")
        return request_timeout
    return _DEFAULT_AGENT_REQUEST_TIMEOUT


class _AgentHTTPClient:
    """Apply E2B-style request and stream timeouts to direct Agent calls."""

    _REQUEST_TIMEOUT_HEADER = "x-conch-sdk-request-timeout"
    _PROCESS_STREAM_HEADER = "x-conch-sdk-process-stream"
    _MISSING_TIMEOUT = object()

    @classmethod
    def _headers(cls, headers):
        copied = dict(headers or {})
        raw_timeout = copied.pop(cls._REQUEST_TIMEOUT_HEADER, cls._MISSING_TIMEOUT)
        process_stream = copied.pop(cls._PROCESS_STREAM_HEADER, None) == "1"
        timeout = None if raw_timeout is cls._MISSING_TIMEOUT else float(raw_timeout)
        return copied, _resolve_request_timeout(timeout), process_stream

    @staticmethod
    def _client(connect_timeout: Optional[float], read_timeout: Optional[float]):
        transport = SyncHTTPTransport(
            connect_timeout=connect_timeout,
            read_timeout=read_timeout,
        )
        return SyncClient(transport), transport

    def get(self, url, headers=None, *, timeout=None, params=None):
        headers, request_timeout, _ = self._headers(headers)
        client, transport = self._client(request_timeout, request_timeout)
        try:
            return client.get(url, headers, timeout=request_timeout, params=params)
        finally:
            transport.close()

    def post(self, url, headers=None, content=None, *, timeout=None, params=None):
        headers, request_timeout, _ = self._headers(headers)
        client, transport = self._client(request_timeout, request_timeout)
        try:
            return client.post(url, headers, content, timeout=request_timeout, params=params)
        finally:
            transport.close()

    @contextmanager
    def stream(self, method, url, headers=None, content=None, *, timeout=None, params=None):
        headers, request_timeout, process_stream = self._headers(headers)
        read_timeout = timeout if process_stream else request_timeout
        stream_timeout = None if process_stream else request_timeout
        client, transport = self._client(request_timeout, read_timeout)
        try:
            with client.stream(method, url, headers, content, timeout=stream_timeout, params=params) as response:
                yield response
        finally:
            transport.close()

    def close(self):
        # Transports are scoped to individual requests and closed by each
        # request method; keep this method for ConnectRPC client cleanup.
        return None


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
        codec = google_protobuf_binary_codec()
        self._rpc_http_client = _AgentHTTPClient()
        self.process_client = agent_connect.ProcessServiceClientSync(
            self.base_url, codec=codec, http_client=self._rpc_http_client
        )
        self.file_client = agent_connect.FileServiceClientSync(
            self.base_url, codec=codec, http_client=self._rpc_http_client
        )

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

    def health_check(self, request_timeout: Optional[float] = None) -> Dict[str, Any]:
        # Do not reuse a direct Agent connection after its sandbox disappears.
        with requests.Session() as session:
            response = session.get(
                self._url("/health"),
                timeout=_resolve_request_timeout(request_timeout),
                headers={"Connection": "close"},
            )
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
        stdin: Optional[Union[str, bytes]] = None,
        timeout_ms: Optional[int] = None,
        request_timeout: Optional[float] = None,
    ) -> Dict[str, Any]:
        request = self._build_start_process_request(
            cmd=cmd,
            cwd=cwd,
            env=env,
            content=content,
            args=args,
            background=background,
            tag=tag,
            pty=pty,
            stdin=stdin,
        )
        raw_events = self._rpc_call(
            self.process_client.start_process, request, timeout_ms=timeout_ms,
            request_timeout=request_timeout, process_stream=True,
        )
        events = self._decode_process_events(self._rpc_iter(raw_events))
        if background:
            return self._start_background_process_response(request, events)
        return self._aggregate_process_response(events)

    def stream_process(
        self,
        cmd: str,
        cwd: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
        content: Optional[str] = None,
        args: Optional[list] = None,
        background: bool = False,
        tag: Optional[str] = None,
        pty: Optional[Dict[str, int]] = None,
        stdin: Optional[Union[str, bytes]] = None,
        timeout_ms: Optional[int] = None,
        request_timeout: Optional[float] = None,
    ) -> Iterator[Dict[str, Any]]:
        request = self._build_start_process_request(
            cmd=cmd,
            cwd=cwd,
            env=env,
            content=content,
            args=args,
            background=background,
            tag=tag,
            pty=pty,
            stdin=stdin,
        )
        raw_events = self._rpc_call(
            self.process_client.start_process, request, timeout_ms=timeout_ms,
            request_timeout=request_timeout, process_stream=True,
        )
        yield from self._decode_process_events(self._rpc_iter(raw_events))

    def _build_start_process_request(
        self,
        cmd: str,
        cwd: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
        content: Optional[str] = None,
        args: Optional[list] = None,
        background: bool = False,
        tag: Optional[str] = None,
        pty: Optional[Dict[str, int]] = None,
        stdin: Optional[Union[str, bytes]] = None,
    ) -> agent_pb2.StartProcessRequest:
        if content is not None and args:
            raise InvalidArgumentError("content cannot be used with args; write a file first and execute it via args")
        if pty is not None and stdin is not None:
            raise InvalidArgumentError("stdin cannot be used with pty")
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
        if stdin is not None:
            request.stdin = stdin.encode() if isinstance(stdin, str) else stdin
        return request

    def connect_process(self, process: Optional[Dict[str, Any]] = None, *, pid: Optional[int] = None,
        tag: Optional[str] = None, request_timeout: Optional[float] = None) -> Iterator[Dict[str, Any]]:
        selector = self._process_selector(process, pid=pid, tag=tag)
        request = agent_pb2.ConnectProcessRequest(process=selector)
        raw_events = self._rpc_call(
            self.process_client.connect, request, request_timeout=request_timeout, process_stream=True
        )
        yield from self._decode_process_events(self._rpc_iter(raw_events))

    def list_processes(self, request_timeout: Optional[float] = None) -> List[Dict[str, Any]]:
        response = self._rpc_call(
            self.process_client.list, agent_pb2.ListProcessesRequest(), request_timeout=request_timeout
        )
        return [self._process_info_to_dict(process) for process in response.processes]

    def send_signal(self, process: Optional[Dict[str, Any]] = None, *, pid: Optional[int] = None,
                    tag: Optional[str] = None, signal: int = 15,
                    request_timeout: Optional[float] = None) -> bool:
        selector = self._process_selector(process, pid=pid, tag=tag)
        request = agent_pb2.SendSignalRequest(process=selector, signal=signal)
        try:
            self._rpc_call(self.process_client.send_signal, request, request_timeout=request_timeout)
        except NotFoundError:
            return False
        return True

    def post_files(self, files: List[Dict[str, Any]], request_timeout: Optional[float] = None) -> Dict[str, Any]:
        uploaded_count = 0
        entries = []
        for file_spec in files:
            response = self._rpc_call(
                self.file_client.post_file_stream,
                self._iter_file_chunk_messages(file_spec),
                request_timeout=request_timeout,
            )
            uploaded_count += response.uploaded_count
            entries.extend(self._write_info_to_dict(entry) for entry in getattr(response, "entries", []))
        return {
            "status": self.STATUS_SUCCESS,
            "uploaded_count": uploaded_count,
            "entries": entries,
            "message": f"uploaded {uploaded_count} files",
        }

    def get_file(self, remote_path: str, local_path: str, request_timeout: Optional[float] = None) -> Dict[str, Any]:
        local_dir = os.path.dirname(local_path) or "."
        os.makedirs(local_dir, exist_ok=True)
        try:
            target_mode = stat.S_IMODE(os.stat(local_path).st_mode)
        except FileNotFoundError:
            target_mode = None
        size = 0
        request = agent_pb2.GetFileRequest(filepath=remote_path)
        chunks = self._rpc_call(self.file_client.get_file_stream, request, request_timeout=request_timeout)
        temp_path = None
        committed = False
        try:
            fd, temp_path = _create_download_temp(local_dir)
            if target_mode is not None:
                os.fchmod(fd, target_mode)
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

    def stream_file(self, remote_path: str, request_timeout: Optional[float] = None) -> Iterator[bytes]:
        request = agent_pb2.GetFileRequest(filepath=remote_path)
        chunks = self._rpc_call(self.file_client.get_file_stream, request, request_timeout=request_timeout)
        for chunk in self._rpc_iter(chunks):
            yield chunk.content

    def read_file(self, remote_path: str, request_timeout: Optional[float] = None) -> bytes:
        return b"".join(self.stream_file(remote_path, request_timeout=request_timeout))

    def get_files(self, mappings: List[Dict[str, str]], request_timeout: Optional[float] = None) -> Dict[str, Any]:
        downloaded = 0
        failed = []
        for item in mappings:
            if "remote" not in item or "local" not in item:
                raise InvalidArgumentError("invalid file mapping, need 'remote' and 'local': " + str(item))
            try:
                if request_timeout is None:
                    self.get_file(item["remote"], item["local"])
                else:
                    self.get_file(item["remote"], item["local"], request_timeout=request_timeout)
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

    def list_files(self, path: str, depth: int = 1, request_timeout: Optional[float] = None) -> List[Dict[str, Any]]:
        request = agent_pb2.ListFilesRequest(path=path, depth=depth)
        response = self._rpc_call(self.file_client.list_files, request, request_timeout=request_timeout)
        return [self._file_entry_to_dict(entry) for entry in response.entries]

    def search_files(self, path: str, pattern: str, exclude_patterns: Optional[List[str]] = None,
                     request_timeout: Optional[float] = None) -> List[Dict[str, Any]]:
        request = agent_pb2.SearchFilesRequest(path=path, pattern=pattern)
        request.exclude_patterns.extend(exclude_patterns or [])
        response = self._rpc_call(self.file_client.search_files, request, request_timeout=request_timeout)
        return [self._file_entry_to_dict(entry) for entry in response.entries]

    def close(self):
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

    def _rpc_call(
        self,
        method,
        request,
        timeout_ms: Optional[int] = None,
        request_timeout: Optional[float] = None,
        process_stream: bool = False,
    ):
        try:
            if timeout_ms is not None and timeout_ms < 0:
                raise InvalidArgumentError("timeout_ms must not be negative")
            headers = self._headers()
            if request_timeout is not None:
                headers[_AgentHTTPClient._REQUEST_TIMEOUT_HEADER] = str(request_timeout)
            if process_stream:
                headers[_AgentHTTPClient._PROCESS_STREAM_HEADER] = "1"
            # Connect applies this deadline to its HTTP transport and emits the
            # corresponding protocol timeout header.
            if timeout_ms:
                return method(request, headers=headers, timeout_ms=timeout_ms)
            return method(request, headers=headers)
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
        events: Iterator[Dict[str, Any]],
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

        event = first_event
        if "start" not in event:
            response = self._aggregate_process_response(chain([first_event], events))
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
            "events": events,
        }

    def _aggregate_process_response(self, events: Iterator[Dict[str, Any]]) -> Dict[str, Any]:
        stdout = ""
        stderr = ""
        exit_code = -1
        error = "process ended without an end event"
        for event in events:
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

    @classmethod
    def _decode_process_events(
        cls,
        events: Iterator[agent_pb2.ProcessEvent],
    ) -> Iterator[Dict[str, Any]]:
        decoders = {
            output: codecs.getincrementaldecoder("utf-8")(errors="replace")
            for output in ("stdout", "stderr", "pty")
        }
        for raw_event in events:
            event = cls._process_event_to_dict(raw_event)
            data = event.get("data")
            if data is not None:
                output, raw = next(iter(data.items()), (None, None))
                if output is None:
                    yield event
                    continue
                text = decoders[output].decode(raw, final=False)
                if text:
                    yield {"data": {output: text}}
                continue
            if "end" in event:
                for output, decoder in decoders.items():
                    text = decoder.decode(b"", final=True)
                    if text:
                        yield {"data": {output: text}}
            yield event

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
