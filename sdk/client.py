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
        }
        
        if filename is None:
            filename = self._infer_script_name(cmd)  

        if content is not None:
            request_kwargs["content"] = content
            if args is None:
                args = [filename]  

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
                elif all(k in item for k in ("local_path", "remote_path")):
                    lp = item["local_path"]
                    if not os.path.exists(lp):
                        raise FileNotFoundError(f"local file not found: {lp}")
                    with open(lp, "rb") as f:
                        content = f.read()
                    filepath = item["remote_path"]
                else:
                    raise ValueError(
                        "invalid file spec, need 'content'+'filepath' or 'local_path'+'remote_path': "
                        + str(item)
                    )
                files.append({"filepath": filepath, "content": content})

        elif len(args) == 2:
            local_path, remote_path = args
            if not os.path.exists(local_path):
                raise FileNotFoundError(f"local file not found: {local_path}")
            with open(local_path, "rb") as f:
                content = f.read()
            files.append({"filepath": remote_path, "content": content})

        elif len(args) == 1 and isinstance(args[0], (list, tuple)):
            for item in args[0]:
                if "content" in item and "filepath" in item:
                    content = item["content"]
                    filepath = item["filepath"]
                elif all(k in item for k in ("local_path", "remote_path")):
                    lp = item["local_path"]
                    if not os.path.exists(lp):
                        raise FileNotFoundError(f"local file not found: {lp}")
                    with open(lp, "rb") as f:
                        content = f.read()
                    filepath = item["remote_path"]
                else:
                    raise ValueError(
                        "invalid file spec, need 'content'+'filepath' or 'local_path'+'remote_path': "
                        + str(item)
                    )
                files.append({"filepath": filepath, "content": content})
        else:
            raise TypeError("usage: post_files(local, remote) or post_files([spec, ...]) or post_files(files=[...])")

        pb_files = [
            agent_pb2.File(filepath=f["filepath"], content=f["content"])
            for f in files
        ]
        request = agent_pb2.PostFilesRequest(files=pb_files)

        try:
            response = self.stub.PostFiles(request)
            status = self.STATUS_SUCCESS if not response.error else self.STATUS_FAILED
            return {
                "status": status,
                "uploaded_count": response.uploaded_count,
                "message": response.error or f"uploaded {response.uploaded_count} files",
                "error": response.error,
            }
        except grpc.RpcError as e:
            raise RuntimeError(f"gRPC failed: [{e.code()}] {e.details()}") from e

    def get_file(self, remote_path: str, local_path: str) -> Dict[str, Any]:
        # download single file from agent server
        request = agent_pb2.GetFileRequest(filepath=remote_path)
        try:
            response = self.stub.GetFile(request)
            if response.error:
                raise RuntimeError(f"download failed: {response.error}")

            os.makedirs(os.path.dirname(local_path) or ".", exist_ok=True)
            with open(local_path, "wb") as f:
                f.write(response.content)

            return {
                "status": self.STATUS_SUCCESS,
                "size": len(response.content),
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
