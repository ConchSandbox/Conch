import os
from typing import Optional, Dict, Any, List
import requests
import json
import uuid
import secrets

# Try relative imports first (when imported as a package), fall back to absolute imports
from .client import AgentClient
from .config_loader import load_config


# API keys
SANDBOX_ID_KEY = "sandbox_id"
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
CFG_API_URL_KEY = "api_url"
CFG_WORKDIR_PREFIX_KEY = "workdir_prefix"
CFG_USE_SNAPSHOT_KEY = "use_snapshot"

# API paths
SANDBOX_CREATE_PATH = "/api/sandbox/create"
SANDBOX_DELETE_PATH = "/api/sandbox/delete"
SANDBOX_PAUSE_PATH = "/api/sandbox/pause"

RANDOM_ID_HEX_BYTES = 12
WORKDIR_UUID_SUFFIX_LEN = 8
UNKNOWN_EXIT_CODE = -1

def generate_random_id(prefix: str = "sandbox_") -> str:
    return prefix + secrets.token_hex(RANDOM_ID_HEX_BYTES)

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
            use_snapshot: bool = False,
            api_url: Optional[str] = None,
            client: Optional["AgentClient"] = None,
            workdir: Optional[str] = None,
            sandbox_id: Optional[str] = None,
            snapshot_id: Optional[str] = None,
            vcpu_num: Optional[int] = None,
            ram_mb: Optional[int] = None,
            config_path: Optional[str] = None,
    ):
        self._config: Dict[str, Any] = load_config(config_path=config_path)
        sandbox_cfg = self._config[CFG_SANDBOX_SECTION]
        self.api_url = api_url.rstrip('/') if api_url else sandbox_cfg[CFG_API_URL_KEY].rstrip('/')
        self.workdir: str = workdir or self._generate_workdir()
        self.use_snapshot = (
            use_snapshot
            if use_snapshot is not None
            else sandbox_cfg[CFG_USE_SNAPSHOT_KEY]
        )

        config_sandbox_id = sandbox_cfg.get(SANDBOX_ID_KEY, "")
        self.sandbox_id = sandbox_id or config_sandbox_id or generate_random_id()

        config_snapshot_id = self._config[CFG_SNAPSHOT_SECTION].get(SNAPSHOT_ID_KEY, "")
        self.snapshot_id = snapshot_id or config_snapshot_id

        self.vm_ip = None
        self.client = client
        self.vcpu_num = vcpu_num
        self.ram_mb = ram_mb

        # Create sandbox and Update client(snapshot or normal mode)
        if self.use_snapshot:
            self.create_by_snapshot()
        else:
            self.create()

    def _generate_workdir(self) -> str:
        return f"{self._config[CFG_SANDBOX_SECTION][CFG_WORKDIR_PREFIX_KEY]}{uuid.uuid4().hex[:WORKDIR_UUID_SUFFIX_LEN]}"

    def _update_client_from_result(self, result: Dict[str, Any]):
        # Initialize/update the AgentClient based on sandbox creation result
        status = result.get(STATUS_KEY)
        server_ip = result.get(IP_KEY)

        if status == "ok" and server_ip:
            self.vm_ip = server_ip
            if self.client:
                try:
                    self.client.close()
                except Exception:
                    pass
            self.client = AgentClient(host=self.vm_ip)
        else:
            error_val = result.get(ERROR_KEY)
            error_msg = str(error_val) if error_val is not None else "Unknown error"
            raise RuntimeError(f"Sandbox creation failed: {error_msg}")

    def create_by_snapshot(self):
        # Create sandbox from pre-defined snapshot
        url = f"{self.api_url}{SANDBOX_CREATE_PATH}"
        snap_config = self._config[CFG_SNAPSHOT_SECTION]
        payload = {
            SNAPSHOT_ID_KEY: self.snapshot_id,
            IMAGE_NAME_KEY: "",
            VMM_NAME_KEY: snap_config[VMM_NAME_KEY],
            SANDBOX_ID_KEY: self.sandbox_id,
            VCPU_NUM_KEY: self.vcpu_num or snap_config[VCPU_NUM_KEY],
            RAM_MB_KEY: self.ram_mb or snap_config[RAM_MB_KEY],
        }

        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result[SANDBOX_ID_KEY] = self.sandbox_id
            result[SNAPSHOT_ID_KEY] = self.snapshot_id
            print(f"Create Sandbox by Snapshot !")
            print(json.dumps(result, indent=4, ensure_ascii=False))

            self._update_client_from_result(result)

        except requests.exceptions.RequestException as e:
            error_msg = response.text if 'response' in locals() else str(e)
            print(f"Failed to create sandbox by snapshot !")
            return {
                STATUS_KEY: "failed",
                SANDBOX_ID_KEY: self.sandbox_id,
                SNAPSHOT_ID_KEY: self.snapshot_id,
                ERROR_KEY: error_msg
            }

    def create(self):
        # Create sandbox from base image
        url = f"{self.api_url}{SANDBOX_CREATE_PATH}"
        img_config = self._config[CFG_IMAGE_SECTION]
        payload = {
            SNAPSHOT_ID_KEY: "",
            IMAGE_NAME_KEY: img_config[IMAGE_NAME_KEY],
            VMM_NAME_KEY: img_config[VMM_NAME_KEY],
            SANDBOX_ID_KEY: self.sandbox_id,
            VCPU_NUM_KEY: self.vcpu_num or img_config[VCPU_NUM_KEY],
            RAM_MB_KEY: self.ram_mb or img_config[RAM_MB_KEY],
        }

        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result[SANDBOX_ID_KEY] = self.sandbox_id
            print(f"Sandbox Created !")
            print(json.dumps(result, indent=4, ensure_ascii=False))

            self._update_client_from_result(result)

        except requests.exceptions.RequestException as e:
            error_msg = response.text if 'response' in locals() else str(e)
            print(f"Failed to create sandbox !")
            return {
                STATUS_KEY: "failed",
                SANDBOX_ID_KEY: self.sandbox_id,
                ERROR_KEY: error_msg
            }

    def delete(self):
        # Delete sandbox
        url = f"{self.api_url}{SANDBOX_DELETE_PATH}"
        payload = {SANDBOX_ID_KEY: self.sandbox_id}

        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result[SANDBOX_ID_KEY] = self.sandbox_id
            print(f"Sandbox Deleted !")
            print(json.dumps(result, indent=4, ensure_ascii=False))

        except requests.exceptions.RequestException as e:
            error_msg = response.text if 'response' in locals() else str(e)
            print(f"Failed to delete sandbox !")
            return {
                STATUS_KEY: "failed",
                SANDBOX_ID_KEY: self.sandbox_id,
                ERROR_KEY: error_msg
            }

    def pause(self):
        # Pause sandbox
        url = f"{self.api_url}{SANDBOX_PAUSE_PATH}"
        payload = {
            SANDBOX_ID_KEY: self.sandbox_id,
        }

        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result[SANDBOX_ID_KEY] = self.sandbox_id
            self.snapshot_id = result.get(SNAPSHOT_ID_RESP_KEY)
            print(f"Sandbox Paused !")
            print(json.dumps(result, indent=4, ensure_ascii=False))

        except requests.exceptions.RequestException as e:
            error_msg = response.text if 'response' in locals() else str(e)
            print(f"Failed to pause sandbox !")
            return {
                STATUS_KEY: "failed",
                SANDBOX_ID_KEY: self.sandbox_id,
                ERROR_KEY: error_msg
            }

    def get_sandbox_id(self) -> str:
        # Return sandbox unique ID
        return self.sandbox_id

    def get_snapshot_id(self) -> str:
        # Return snapshot unique ID
        return self.snapshot_id

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
