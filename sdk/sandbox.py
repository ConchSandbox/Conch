import os
from typing import Optional, Dict, Any, List
from dataclasses import dataclass
from urllib.parse import quote, quote_plus
import requests
import requests_unixsocket
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
SANDBOX_COLLECTION_PATH = "/api/v1/sandboxes"
SANDBOX_INSTANCE_PATH_TEMPLATE = "/api/v1/sandboxes/{sandbox_id}"

SANDBOX_CREATE_PATH = SANDBOX_COLLECTION_PATH
SANDBOX_LIST_PATH = SANDBOX_COLLECTION_PATH
SANDBOX_GET_PATH_TEMPLATE = SANDBOX_INSTANCE_PATH_TEMPLATE
SANDBOX_DELETE_PATH_TEMPLATE = SANDBOX_INSTANCE_PATH_TEMPLATE

# TODO: migrate pause to a v1 direct API once its endpoint is finalized.
SANDBOX_PAUSE_PATH = "/api/sandbox/pause"

SERVICE_HEALTH_PATH = "/health"
HTTP_NO_CONTENT = 204
SANDBOXES_KEY = "sandboxes"
SANDBOX_KEY = "sandbox"
SANDBOX_LOGS_PATH_TEMPLATE = "/api/v1/sandboxes/{sandbox_id}/logs"
LOGS_KEY = "logs"
SANDBOX_NETWORK_PATH_TEMPLATE = "/api/v1/sandboxes/{sandbox_id}/network"

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

    def _get_control_plane_response(self, path: str):
        url = self._build_control_plane_url(path)
        response = self._session.get(url)
        response.raise_for_status()
        return response
    def _get_control_plane_requests(
        self,
        path: str,
        params: Optional[Dict[str, Any]] = None,
        ) -> Dict[str, Any]:
        url = self._build_control_plane_url(path)
        response = self._session.get(url, params=params)
        response.raise_for_status()

        if response.status_code == 204:
            return {}

        return response.json()

    def _put_control_plane_requests(
        self,
        path: str,
        payload: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        url = self._build_control_plane_url(path)
        response = self._session.put(url, json=payload or {})
        response.raise_for_status()

        if response.status_code == 204:
            return {}

        return response.json()

    def _delete_control_plane_requests(
        self,
        path: str,
        params: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        url = self._build_control_plane_url(path)
        response = self._session.delete(url, params=params)
        response.raise_for_status()

        if response.status_code == 204:
            return {}

        return response.json()

    @staticmethod
    def _extract_sandbox_record(result: Dict[str, Any]) -> Dict[str, Any]:
        sandbox = result.get(SANDBOX_KEY)
        if isinstance(sandbox, dict):
            return sandbox
        return result

    def _apply_sandbox_record(self, record: Dict[str, Any]) -> None:
        if not isinstance(record, dict):
            return

        self.sandbox_id = record.get(SANDBOX_ID_KEY) or self.sandbox_id
        self.namespace = record.get(NAMESPACE_KEY) or self.namespace
        self.ip = record.get(IP_KEY) or self.ip
        self.snapshot_id = record.get(SNAPSHOT_ID_KEY) or self.snapshot_id
        self.image_name = record.get(IMAGE_NAME_KEY) or self.image_name
        self.vcpu_num = record.get(VCPU_NUM_KEY) or self.vcpu_num
        self.vcpu_max = record.get(VCPU_MAX_KEY) or self.vcpu_max
        self.ram_mb = record.get(RAM_MB_KEY) or self.ram_mb

        agent_token = record.get(AGENT_TOKEN_KEY)
        if self.ip and agent_token:
            self.agent_token = agent_token
            if self.client:
                try:
                    self.client.close()
                except Exception:
                    pass
            self.client = AgentClient(host=self.ip, token=self.agent_token)

    def _use_snapshot_startup(self) -> bool:
        return bool(self.snapshot_id) or self.use_snapshot

    def _startup_config(self) -> Dict[str, Any]:
        if self._use_snapshot_startup():
            return self._config[CFG_SNAPSHOT_SECTION]
        return self._config[CFG_IMAGE_SECTION]

    def _get_vmm_name(self) -> str:
        config = self._startup_config()
        return config.get(VMM_NAME_KEY, "")

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
        if not target_id:
            raise RuntimeError("Cannot delete sandbox without sandbox_id")

        params: Dict[str, Any] = {}
        if self.namespace:
            params[NAMESPACE_KEY] = self.namespace

        path = SANDBOX_DELETE_PATH_TEMPLATE.format(
            sandbox_id=quote(str(target_id), safe="")
        )

        try:
            self._delete_control_plane_requests(
                path,
                params=params if params else None,
            )
            return True

        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    @classmethod
    def service_health(
            cls,
            unix_socket: Optional[str] = None,
            api_url: Optional[str] = None,
            config_path: Optional[str] = None,
    ) -> bool:
        sbx = cls(
            unix_socket=unix_socket,
            api_url=api_url,
            config_path=config_path,
        )

        try:
            response = sbx._get_control_plane_response(SERVICE_HEALTH_PATH)
            return response.status_code == HTTP_NO_CONTENT
        except requests.exceptions.RequestException:
            return False

    @classmethod
    def list(
            cls,
            unix_socket: Optional[str] = None,
            api_url: Optional[str] = None,
            namespace: Optional[str] = None,
            config_path: Optional[str] = None,
    ) -> List[Dict[str, Any]]:
        sbx = cls(
            unix_socket=unix_socket,
            api_url=api_url,
            namespace=namespace,
            config_path=config_path,
        )

        params: Dict[str, Any] = {}
        if sbx.namespace:
            params[NAMESPACE_KEY] = sbx.namespace

        try:
            result = sbx._get_control_plane_requests(
                SANDBOX_LIST_PATH,
                params=params if params else None,
            )
        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

        sandboxes = result.get(SANDBOXES_KEY, [])
        if isinstance(sandboxes, list):
            return sandboxes

        return []

    @classmethod
    def get(
            cls,
            sandbox_id: str,
            unix_socket: Optional[str] = None,
            api_url: Optional[str] = None,
            namespace: Optional[str] = None,
            config_path: Optional[str] = None,
    ) -> "Sandbox":
        if not sandbox_id:
            raise RuntimeError("Cannot get sandbox without sandbox_id")

        sbx = cls(
            sandbox_id=sandbox_id,
            unix_socket=unix_socket,
            api_url=api_url,
            namespace=namespace,
            config_path=config_path,
        )

        params: Dict[str, Any] = {}
        if sbx.namespace:
            params[NAMESPACE_KEY] = sbx.namespace

        path = SANDBOX_GET_PATH_TEMPLATE.format(
            sandbox_id=quote(str(sandbox_id), safe="")
        )

        try:
            result = sbx._get_control_plane_requests(
                path,
                params=params if params else None,
            )
        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

        sbx._apply_sandbox_record(sbx._extract_sandbox_record(result))
        return sbx

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

    def update_network(
        self,
        allow_out: Optional[bool] = None,
        deny_out: Optional[List[str]] = None,
        egress_proxy: Optional[str] = None,
        allow_public_traffic: Optional[bool] = None,
    ) -> Dict[str, Any]:
        if not self.sandbox_id:
            raise RuntimeError("Cannot update sandbox network without sandbox_id")

        payload: Dict[str, Any] = {}

        if allow_out is not None:
            payload["allowOut"] = allow_out

        if deny_out is not None:
            payload["denyOut"] = deny_out

        if egress_proxy is not None:
            payload["egressProxy"] = egress_proxy

        if allow_public_traffic is not None:
            payload["allowPublicTraffic"] = allow_public_traffic

        path = SANDBOX_NETWORK_PATH_TEMPLATE.format(
            sandbox_id=quote(str(self.sandbox_id), safe="")
        )

        try:
            return self._put_control_plane_requests(path, payload)
        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

    def logs(
        self,
        cursor: Optional[str] = None,
        limit: Optional[int] = None,
        level: Optional[str] = None,
        search: Optional[str] = None,
) -> Dict[str, Any]:
        if not self.sandbox_id:
            raise RuntimeError("Cannot get sandbox logs without sandbox_id")

        params: Dict[str, Any] = {}

        if cursor is not None:
            params["cursor"] = cursor

        if limit is not None:
            params["limit"] = limit

        if level is not None:
            params["level"] = level

        if search is not None:
            params["search"] = search

        path = SANDBOX_LOGS_PATH_TEMPLATE.format(
            sandbox_id=quote(str(self.sandbox_id), safe="")
        )

        try:
            return self._get_control_plane_requests(
                path,
                params=params if params else None,
            )
        except requests.exceptions.RequestException as e:
            raise RuntimeError(_request_exception_message(e))

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