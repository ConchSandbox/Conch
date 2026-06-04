import grpc
from typing import Dict, Any, Optional
import os
import sys

PROJECT_ROOT = os.path.dirname(os.path.abspath(__file__))
PROJECT_ROOT = os.path.dirname(PROJECT_ROOT)
sys.path.insert(0, PROJECT_ROOT)

from api.py_proto import agent_pb2
from api.py_proto import agent_pb2_grpc

class AgentClient:
    DEFAULT_GRPC_PORT = 4064
    STATUS_SUCCESS = 0
    STATUS_FAILED = -1
    FILE_CHUNK_SIZE = 1024 * 1024
    DEFAULT_SCRIPT_NAMES = {
        ("python", "python3", "python2"): "main.py",
        ("node", "nodejs"): "main.js",
        ("bash", "sh", "zsh"): "main.sh",
        ("lua",): "main.lua",
        ("ruby", "rb"): "main.rb",
    }
    FALLBACK_SCRIPT_NAME = "main.py"

    def __init__(self, host: str, port: int = DEFAULT_GRPC_PORT):
        self.host = host
        self.port = port
        self.address = f"{host}:{port}"
        self.channel = None
        self.stub = None
        self._connect()

    def _connect(self):
        # connect to agent grpc server
        try:
            self.channel = grpc.insecure_channel(self.address)
            self.stub = agent_pb2_grpc.AgentServiceStub(self.channel)
        except Exception as e:
            raise ConnectionError(f"failed to connect Agent server {self.address}: {e}") from e

    def health_check(self) -> Dict[str, Any]:
        request = agent_pb2.Empty()
        try:
            response = self.stub.HealthCheck(request)
            return {
                "status": "OK",
                "message": response.message,
            }
        except grpc.RpcError as e:
            raise RuntimeError(f"healthcheck failed: [{e.code()}] {e.details()}") from e

    def start_process(
        self,
        cmd: str,
        cwd: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
        content: Optional[str] = None,
        filename: Optional[str] = None,
        args: Optional[list] = None,
    ) -> Dict[str, Any]:
        request_kwargs = {
            "cmd": cmd,
            "cwd": cwd,
            "env": env or {},
            "args": args or [],
        }

        if content is not None:
            request_kwargs["content"] = content
            if args is None:
                request_kwargs["args"] = []

        request = agent_pb2.StartProcessRequest(**request_kwargs)

        try:
            response = self.stub.StartProcess(request)
            status = self.STATUS_SUCCESS if response.exit_code == 0 and not response.error else self.STATUS_FAILED
            return {
                "status": status,
                "message": response.error or "OK",
                "stdout": response.stdout,
                "stderr": response.stderr,
                "exit_code": response.exit_code,
                "error": response.error,
            }
        except grpc.RpcError as e:
            raise RuntimeError(f"command execution failed: [{e.code()}] {e.details()}") from e

    def post_files(self, *args, **kwargs) -> Dict[str, Any]:
        # upload files to agent server
        files = []

        if 'files' in kwargs:
            if args:
                raise TypeError("cannot mix args and 'files' keyword")
            file_specs = kwargs['files']
            if not isinstance(file_specs, (list, tuple)):
                raise TypeError("files must be a list or tuple")

            for item in file_specs:
                if not isinstance(item, dict):
                    raise ValueError(f"invalid file spec: {item}")

                if "content" in item and "filepath" in item:
                    content = item["content"]
                    filepath = item["filepath"]
                    files.append({"filepath": filepath, "content": content})
                elif all(k in item for k in ("local_path", "remote_path")):
                    lp = item["local_path"]
                    if not os.path.exists(lp):
                        raise FileNotFoundError(f"local file not found: {lp}")
                    filepath = item["remote_path"]
                    files.append({"filepath": filepath, "local_path": lp})
                else:
                    raise ValueError(
                        "invalid file spec, need 'content'+'filepath' or 'local_path'+'remote_path': "
                        + str(item)
                    )

        elif len(args) == 2:
            local_path, remote_path = args
            if not os.path.exists(local_path):
                raise FileNotFoundError(f"local file not found: {local_path}")
            files.append({"filepath": remote_path, "local_path": local_path})

        elif len(args) == 1 and isinstance(args[0], (list, tuple)):
            for item in args[0]:
                if "content" in item and "filepath" in item:
                    content = item["content"]
                    filepath = item["filepath"]
                    files.append({"filepath": filepath, "content": content})
                elif all(k in item for k in ("local_path", "remote_path")):
                    lp = item["local_path"]
                    if not os.path.exists(lp):
                        raise FileNotFoundError(f"local file not found: {lp}")
                    filepath = item["remote_path"]
                    files.append({"filepath": filepath, "local_path": lp})
                else:
                    raise ValueError(
                        "invalid file spec, need 'content'+'filepath' or 'local_path'+'remote_path': "
                        + str(item)
                    )
        else:
            raise TypeError("usage: post_files(local, remote) or post_files([spec, ...]) or post_files(files=[...])")

        uploaded_count = 0
        try:
            for file in files:
                response = self.stub.PostFileStream(self._iter_file_chunks(file))
                if response.error:
                    return {
                        "status": self.STATUS_FAILED,
                        "uploaded_count": uploaded_count,
                        "message": response.error,
                        "error": response.error,
                    }
                uploaded_count += response.uploaded_count
            return {
                "status": self.STATUS_SUCCESS,
                "uploaded_count": uploaded_count,
                "message": f"uploaded {uploaded_count} files",
                "error": "",
            }
        except grpc.RpcError as e:
            raise RuntimeError(f"gRPC failed: [{e.code()}] {e.details()}") from e

    def get_file(self, remote_path: str, local_path: str) -> Dict[str, Any]:
        # download single file from agent server
        request = agent_pb2.GetFileRequest(filepath=remote_path)
        try:
            os.makedirs(os.path.dirname(local_path) or ".", exist_ok=True)
            size = 0
            with open(local_path, "wb") as f:
                for chunk in self.stub.GetFileStream(request):
                    f.write(chunk.content)
                    size += len(chunk.content)

            return {
                "status": self.STATUS_SUCCESS,
                "size": size,
                "message": "OK",
            }
        except grpc.RpcError as e:
            raise RuntimeError(f"gRPC failed: [{e.code()}] {e.details()}") from e

    def get_files(self, mappings: list[Dict[str, str]]) -> Dict[str, Any]:
        # batch download files from agent server
        downloaded = 0
        failed = []

        for item in mappings:
            try:
                self.get_file(item["remote"], item["local"])
                downloaded += 1
            except Exception as e:
                failed.append({"remote": item["remote"], "local": item["local"], "error": str(e)})

        status = self.STATUS_SUCCESS if not failed else self.STATUS_FAILED
        return {
            "status": status,
            "downloaded_count": downloaded,
            "failed": failed,
            "message": f"Downloaded {downloaded}, failed {len(failed)}"
        }

    def close(self):
        # close grpc channel
        if self.channel:
            self.channel.close()

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        self.close()

    def _infer_script_name(self, cmd: str) -> str:
        # Infer default script name by command
        cmd = cmd.lower()
        for cmd_set, script_name in self.DEFAULT_SCRIPT_NAMES.items():
            if cmd in cmd_set:
                return script_name
        return self.FALLBACK_SCRIPT_NAME

    def _iter_file_chunks(self, file: Dict[str, Any]):
        filepath = file["filepath"]
        first = True

        if "local_path" in file:
            with open(file["local_path"], "rb") as f:
                while True:
                    content = f.read(self.FILE_CHUNK_SIZE)
                    if not content:
                        break
                    yield agent_pb2.FileChunk(filepath=filepath if first else "", content=content)
                    first = False
            if first:
                yield agent_pb2.FileChunk(filepath=filepath, content=b"")
            return

        content = file["content"]
        if isinstance(content, str):
            content = content.encode()
        for offset in range(0, len(content), self.FILE_CHUNK_SIZE):
            yield agent_pb2.FileChunk(
                filepath=filepath if first else "",
                content=content[offset:offset + self.FILE_CHUNK_SIZE],
            )
            first = False
        if first:
            yield agent_pb2.FileChunk(filepath=filepath, content=b"")
