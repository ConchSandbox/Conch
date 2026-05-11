import os
from typing import Optional, Dict, Any, List
from dataclasses import dataclass
from urllib.parse import quote_plus
import requests
import requests_unixsocket
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
USE_SNAPSHOT_KEY = "use_snapshot"
VMM_NAME_KEY = "vmm_name"
VCPU_NUM_KEY = "vcpu_num"
VCPU_MAX_KEY = "vcpu_max"
RAM_MB_KEY = "ram_mb"
STATUS_KEY = "status"
ERROR_KEY = "error"
MESSAGE_KEY = "message"
IP_KEY = "ip"
AGENT_TOKEN_KEY = "agent_token"
SNAPSHOT_ID_RESP_KEY = "snapshotId"

# Config keys
CFG_SANDBOX_SECTION = "sandbox"
CFG_SNAPSHOT_SECTION = "snapshot"
CFG_IMAGE_SECTION = "image"
CFG_UNIX_SOCKET_KEY = "unix_socket"
CFG_API_URL_KEY = "api_url"
CFG_USE_SNAPSHOT_KEY = "use_snapshot"

# API paths
SANDBOX_CREATE_PATH = "/api/sandbox/create"
SANDBOX_DELETE_PATH = "/api/sandbox/delete"
SANDBOX_PAUSE_PATH = "/api/sandbox/pause"

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
class SnapshotInfo:
    snapshot_id: str
    sandbox_id: str


@dataclass
class SandboxInfo:
    # TODO: Extend this with more sandbox metadata once the SDK surface is finalized.
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
            unix_socket: Optional[str] = None,
            api_url: Optional[str] = None,
            sandbox_id: Optional[str] = None,
            image_name: Optional[str] = None,
            namespace: Optional[str] = None,
            snapshot_id: Optional[str] = None,
            vcpu_num: Optional[int] = None,
            vcpu_max: Optional[int] = None,
            ram_mb: Optional[int] = None,
            config_path: Optional[str] = None,
            use_snapshot: Optional[bool] = None,
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
        self.image_name = image_name or self._config[CFG_IMAGE_SECTION][IMAGE_NAME_KEY]

        config_snapshot_id = self._config[CFG_SNAPSHOT_SECTION].get(SNAPSHOT_ID_KEY, "")
        self.snapshot_id = snapshot_id or config_snapshot_id
        config_use_snapshot = self._config[CFG_IMAGE_SECTION].get(CFG_USE_SNAPSHOT_KEY, False)
        self.use_snapshot = bool(config_use_snapshot if use_snapshot is None else use_snapshot)

        self.ip = None
        self.agent_token = None
        self.client = None
        self.vcpu_num = vcpu_num
        self.vcpu_max = vcpu_max
        self.ram_mb = ram_mb


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

    def _use_snapshot_startup(self) -> bool:
        return bool(self.snapshot_id) or self.use_snapshot

    def _startup_config(self) -> Dict[str, Any]:
        if self._use_snapshot_startup():
            return self._config[CFG_SNAPSHOT_SECTION]
        return self._config[CFG_IMAGE_SECTION]

    def _build_create_payload(self) -> Dict[str, Any]:
        if not self.snapshot_id and not self.image_name:
            raise ValueError("image_name is required when snapshot_id is empty")

        config = self._startup_config()
        use_snapshot_image = bool(self.use_snapshot and not self.snapshot_id)
        return {
            NAMESPACE_KEY: self.namespace,
            SNAPSHOT_ID_KEY: self.snapshot_id or "",
            IMAGE_NAME_KEY: self.image_name if not self.snapshot_id else "",
            USE_SNAPSHOT_KEY: use_snapshot_image,
            VMM_NAME_KEY: config[VMM_NAME_KEY],
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

    def pause(self):
        # Pause sandbox.
        # TODO: Revisit the current pause lifecycle so snapshotting is not tightly coupled to sandbox deletion.
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
            raise RuntimeError(_request_exception_message(e))

    @classmethod
    def create(cls, snapshot_id: Optional[str] = None, **kwargs) -> "Sandbox":
        sbx = cls(snapshot_id=snapshot_id, **kwargs)
        return sbx._do_create()
    
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
        args = kwargs.pop('args', [])
        env = kwargs.pop('env', {})

        request_kwargs = {
            "cmd": cmd,
            "cwd": cwd,
            "env": env,
            "args": args,
        }
        if content is not None:
            request_kwargs["content"] = content
            if not args:
                request_kwargs["args"] = []

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
        # Upload files to sandbox
        files = []
        if len(args) == 2:
            local_path, remote_path = args
            if not os.path.exists(local_path):
                return {STATUS_KEY: AgentClient.STATUS_FAILED, MESSAGE_KEY: f"Local file not found: {local_path}"}
            if not os.path.isfile(local_path):
                return {STATUS_KEY: AgentClient.STATUS_FAILED, MESSAGE_KEY: f"Not a file: {local_path}"}
            with open(local_path, "rb") as f:
                content = f.read()
            files.append({"filepath": remote_path, "content": content})

        elif len(args) == 1 and isinstance(args[0], (list, tuple)):
            file_specs = args[0]
            for item in file_specs:
                if not isinstance(item, dict) or "filepath" not in item or "content" not in item:
                    return {STATUS_KEY: AgentClient.STATUS_FAILED, MESSAGE_KEY: f"Invalid file spec: {item}"}
                files.append({"filepath": item["filepath"], "content": item["content"]})

        else:
            return {
                STATUS_KEY: AgentClient.STATUS_FAILED,
                MESSAGE_KEY: "Invalid call. Usage: upload(local, remote) or upload([spec, ...])",
            }

        return self.client.post_files(files=files, **kwargs)

    def download(self, remote_path: str, local_path: str, **kwargs) -> Dict[str, Any]:
        # Download file from sandbox
        return self.client.get_file(remote_path=remote_path, local_path=local_path, **kwargs)

    def list_files(self, path: Optional[str] = None) -> List[str]:
        # List all files in sandbox directory
        target_path = path if path is not None else "."
        res = self.execute(cmd="sh", args=["-c", f"find {target_path} -type f || echo 'find not available'"])
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
