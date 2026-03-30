import os
from typing import Optional, Dict, Any, List
from dataclasses import dataclass
import requests
import requests_unixsocket
import json
import uuid
import secrets

# Try relative imports first (when imported as a package), fall back to absolute imports
from .client import AgentClient
from .config_loader import load_config


# API keys
SANDBOX_ID_KEY = "sandbox_id"
NAMESPACE_KEY = "namespace"
SNAPSHOT_ID_KEY = "snapshot_id"
IMAGE_NAME_KEY = "image_name"
VMM_NAME_KEY = "vmm_name"
VCPU_NUM_KEY = "vcpu_num"
RAM_MB_KEY = "ram_mb"
STATUS_KEY = "status"
ERROR_KEY = "error"
MESSAGE_KEY = "message"
IP_KEY = "ip"
SNAPSHOT_ID_RESP_KEY = "snapshotId"

# Config keys
CFG_SANDBOX_SECTION = "sandbox"
CFG_SNAPSHOT_SECTION = "snapshot"
CFG_IMAGE_SECTION = "image"
CFG_UNIX_SOCKET_KEY = "unix_socket"
CFG_API_URL_KEY = "api_url"
CFG_WORKDIR_PREFIX_KEY = "workdir_prefix"

# API paths
SANDBOX_CREATE_PATH = "/api/sandbox/create"
SANDBOX_DELETE_PATH = "/api/sandbox/delete"
SANDBOX_PAUSE_PATH = "/api/sandbox/pause"

RANDOM_ID_HEX_BYTES = 12
WORKDIR_UUID_SUFFIX_LEN = 8
UNKNOWN_EXIT_CODE = -1

def generate_random_id(prefix: str = "sandbox_") -> str:
    return prefix + secrets.token_hex(RANDOM_ID_HEX_BYTES)

@dataclass
class SnapshotInfo:
    snapshot_id: str
    sandbox_id: str


@dataclass
class SandboxInfo:
    sandbox_id: str
    ip: str
    snapshot_id: Optional[str]


class Execution:
    # Store execution output/exit code
    def __init__(self, data: Dict[str, Any]):
        self.stdout = data.get("stdout", "")
        self.stderr = data.get("stderr", "")
        self.exit_code = data.get("exit_code", UNKNOWN_EXIT_CODE)
        self.logs = self.stdout + self.stderr

    def __str__(self):
        return self.logs.strip()

class Sandbox:
    def __init__(
            self,
            api_url: Optional[str] = None,
            workdir: Optional[str] = None,
            sandbox_id: Optional[str] = None,
            image_name: Optional[str] = None,
            namespace: Optional[str] = None,
            snapshot_id: Optional[str] = None,
            vcpu_num: Optional[int] = None,
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
        self.workdir: str = workdir or self._generate_workdir()

        config_sandbox_id = sandbox_cfg.get(SANDBOX_ID_KEY, "")
        self.sandbox_id = sandbox_id or config_sandbox_id or generate_random_id()
        self.namespace = namespace or ""
        self.image_name = image_name or self._config[CFG_IMAGE_SECTION][IMAGE_NAME_KEY]

        config_snapshot_id = self._config[CFG_SNAPSHOT_SECTION].get(SNAPSHOT_ID_KEY, "")
        self.snapshot_id = snapshot_id or config_snapshot_id

        self.ip = None
        self.client = None
        self.vcpu_num = vcpu_num
        self.ram_mb = ram_mb

    def _generate_workdir(self) -> str:
        return f"{self._config[CFG_SANDBOX_SECTION][CFG_WORKDIR_PREFIX_KEY]}{uuid.uuid4().hex[:WORKDIR_UUID_SUFFIX_LEN]}"

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

    def _update_client_from_result(self, result: Dict[str, Any]):
        # Initialize/update the AgentClient based on sandbox creation result
        status = result.get(STATUS_KEY)
        server_ip = result.get(IP_KEY)

        if status == "ok" and server_ip:
            self.ip = server_ip
            if self.client:
                try:
                    self.client.close()
                except Exception:
                    pass
            self.client = AgentClient(host=self.ip)
        else:
            error_val = result.get(ERROR_KEY)
            error_msg = str(error_val) if error_val is not None else "Unknown error"
            raise RuntimeError(f"Sandbox creation failed: {error_msg}")

    def _do_create(self):
        url = f"{self.api_url}{SANDBOX_CREATE_PATH}"
        
        if self.snapshot_id:
            config = self._config[CFG_SNAPSHOT_SECTION]
        else:
            config = self._config[CFG_IMAGE_SECTION]
        
        payload = {
            NAMESPACE_KEY: self.namespace,
            SNAPSHOT_ID_KEY: self.snapshot_id or "",
            IMAGE_NAME_KEY: self.image_name if not self.snapshot_id else "",
            VMM_NAME_KEY: config[VMM_NAME_KEY],
            SANDBOX_ID_KEY: self.sandbox_id,
            VCPU_NUM_KEY: self.vcpu_num or config[VCPU_NUM_KEY],
            RAM_MB_KEY: self.ram_mb or config[RAM_MB_KEY],
        }

        try:
            result = self._post_control_plane_requests(SANDBOX_CREATE_PATH, payload)
            result[SANDBOX_ID_KEY] = self.sandbox_id
            self._update_client_from_result(result)
            return self

        except requests.exceptions.RequestException as e:
            error_msg = e.response.text if hasattr(e, 'response') else str(e)
            raise RuntimeError(error_msg)

    def delete(self, sandbox_id: Optional[str] = None):
        url = f"{self.api_url}{SANDBOX_DELETE_PATH}"
        target_id = sandbox_id if sandbox_id else self.sandbox_id
        payload = {
            NAMESPACE_KEY: self.namespace,
            SANDBOX_ID_KEY: target_id,
        }
        
        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result[SANDBOX_ID_KEY] = target_id
            return True

        except requests.exceptions.RequestException as e:
            error_msg = e.response.text if hasattr(e, 'response') else str(e)
            raise RuntimeError(error_msg)

    @staticmethod
    def delete_sandbox(sandbox_id: str):
        _config = load_config()
        api_url = _config[CFG_SANDBOX_SECTION][CFG_API_URL_KEY].rstrip('/')
        
        url = f"{api_url}{SANDBOX_DELETE_PATH}"
        payload = {
            SANDBOX_ID_KEY: sandbox_id,
        }
        
        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result[SANDBOX_ID_KEY] = sandbox_id
            return True

        except requests.exceptions.RequestException as e:
            error_msg = e.response.text if hasattr(e, 'response') else str(e)
            raise RuntimeError(error_msg)

    def pause(self):
        # Pause sandbox
        payload = {
            NAMESPACE_KEY: self.namespace,
            SANDBOX_ID_KEY: self.sandbox_id,
        }

        try:
            result = self._post_control_plane_requests(SANDBOX_PAUSE_PATH, payload)
            result[SANDBOX_ID_KEY] = self.sandbox_id
            self.snapshot_id = result.get(SNAPSHOT_ID_RESP_KEY)
            return SnapshotInfo(
                snapshot_id=self.snapshot_id,
                sandbox_id=self.sandbox_id
            )

        except requests.exceptions.RequestException as e:
            error_msg = getattr(e.response, 'text', str(e)) if hasattr(e, 'response') else str(e)
            raise RuntimeError(error_msg)

    @classmethod
    def create(cls, snapshot_id: Optional[str] = None, **kwargs) -> "Sandbox":
        sbx = cls(snapshot_id=snapshot_id, **kwargs)
        try:
            sbx._do_create()
        except Exception as e:
            raise RuntimeError(e) from None
        return sbx
    
    def get_info(self) -> SandboxInfo:
        return SandboxInfo(
            sandbox_id=self.sandbox_id,
            ip=self.ip if self.ip else "",
            snapshot_id=self.snapshot_id,
        )
    
    def execute(
            self,
            cmd: str,
            content: Optional[str] = None,
            cwd: Optional[str] = None,
            **kwargs
    ) -> Execution:
        # Execute command in sandbox
        final_cwd = cwd if cwd is not None else self.workdir
        args = kwargs.pop('args', [])
        env = kwargs.pop('env', {})
        timeout = kwargs.pop('timeout', None)
        user = kwargs.pop('user', None)

        request_kwargs = {
            "cmd": cmd,
            "cwd": final_cwd,
            "env": env,
            "args": args,
        }
        if content is not None:
            request_kwargs["content"] = content
            if not args:
                filename = kwargs.get("filename", "main.py")
                request_kwargs["args"] = [filename]
        if timeout is not None:
            request_kwargs["timeout"] = timeout
        if user is not None:
            request_kwargs["user"] = user

        result = self.client.start_process(**request_kwargs)
        return Execution(result)

    def health_check(self) -> Dict[str, Any]:
        # Check sandbox health status
        try:
            return self.client.health_check()
        except Exception as e:
            return {
                STATUS_KEY: "ERROR",
                MESSAGE_KEY: f"Health check failed: {e}"
            }

    def upload(self, *args, **kwargs) -> Dict[str, Any]:
        # Upload files to sandbox working dir
        files = []
        if len(args) == 2:
            local_path, remote_path = args
            full_remote = f"{self.workdir}/{remote_path.lstrip('/')}"
            if not os.path.exists(local_path):
                return {STATUS_KEY: AgentClient.STATUS_FAILED, MESSAGE_KEY: f"Local file not found: {local_path}"}
            if not os.path.isfile(local_path):
                return {STATUS_KEY: AgentClient.STATUS_FAILED, MESSAGE_KEY: f"Not a file: {local_path}"}
            with open(local_path, "rb") as f:
                content = f.read()
            files.append({"filepath": full_remote, "content": content})

        elif len(args) == 1 and isinstance(args[0], (list, tuple)):
            file_specs = args[0]
            for item in file_specs:
                if not isinstance(item, dict) or "filepath" not in item or "content" not in item:
                    return {STATUS_KEY: AgentClient.STATUS_FAILED, MESSAGE_KEY: f"Invalid file spec: {item}"}
                remote_path = item["filepath"]
                content = item["content"]
                full_remote = f"{self.workdir}/{remote_path.lstrip('/')}"
                files.append({"filepath": full_remote, "content": content})

        else:
            return {
                STATUS_KEY: AgentClient.STATUS_FAILED,
                MESSAGE_KEY: "Invalid call. Usage: upload(local, remote) or upload([spec, ...])",
            }

        return self.client.post_files(files=files, **kwargs)

    def download(self, remote_path: str, local_path: str, **kwargs) -> Dict[str, Any]:
        # Download file from sandbox working dir
        full_remote = f"{self.workdir}/{remote_path.lstrip('/')}"
        return self.client.get_file(remote_path=full_remote, local_path=local_path, **kwargs)

    def list_files(self) -> List[str]:
        # List all files in sandbox working dir
        res = self.execute(cmd="sh", args=["-c", "find . -type f || echo 'find not available'"])
        stdout = res.stdout.strip()
        stderr = res.stderr.strip()
        exit_code = res.exit_code

        if exit_code != 0:
            print(f"list_files: failed (exit_code={exit_code})")
            if stderr:
                print(f"stderr: {stderr}")
            return []

        files = [line.strip() for line in stdout.splitlines() if line.strip()]
        return [f for f in files if f != "find not available"]

    def __enter__(self) -> 'Sandbox':
        # Context manager entry (return self)
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        # Context manager exit (auto-delete sandbox)
        try:
            self.delete()
        except Exception as e:
            print(f"Warning: Failed to delete sandbox during exit: {e}")
