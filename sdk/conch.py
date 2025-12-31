import uuid
import os
from typing import Optional, Dict, Any, List
import requests
import json
from client import AgentClient
from config_loader import config

def generate_random_id(prefix: str, length: int = 8) -> str:
    random_part = uuid.uuid4().hex[:length]
    return f"{prefix}{random_part}"

class Execution:
    # Store execution output/exit code
    def __init__(self, data: Dict[str, Any]):
        self.stdout = data.get("stdout", "")
        self.stderr = data.get("stderr", "")
        self.exit_code = data.get("exit_code", -1)
        self.logs = self.stdout + self.stderr

    def __str__(self):
        return self.logs.strip()

class Sandbox:
    def __init__(
            self,
            api_url: Optional[str] = None,
            client: Optional["AgentClient"] = None,
            workdir: Optional[str] = None,
            sandbox_id: Optional[str] = None,
            snapshot_id: Optional[str] = None,
            use_snapshot: bool = False
    ):
        # Init core sandbox config (URL/workdir/IDs)
        self.api_url = api_url.rstrip('/') if api_url else config["sandbox"]["api_url"].rstrip('/')
        self.workdir: str = workdir or self._generate_workdir()
        self.use_snapshot = use_snapshot if use_snapshot is not None else config["sandbox"]["use_snapshot"]

        config_sandbox_id = config["sandbox"].get("sandbox_id", "")
        self.sandbox_id = sandbox_id or config_sandbox_id or generate_random_id(prefix="sandbox_")

        config_snapshot_id = config["snapshot"]["snapshot_id"]
        self.snapshot_id = snapshot_id or config_snapshot_id or generate_random_id(prefix="snapshot_")

        self.vm_ip = None
        self.client = client

        # Create sandbox and Update client(snapshot or normal mode)
        if use_snapshot:
            self._update_client_from_result(self.create_by_snapshot())
        else:
            self._update_client_from_result(self.create())

    def _generate_workdir(self) -> str:
        return f"{config['sandbox']['workdir_prefix']}{uuid.uuid4().hex[:8]}"

    def _update_client_from_result(self, result: Dict[str, Any]):
        # Initialize/update the AgentClient based on sandbox creation result
        status = result.get("status")
        server_ip = result.get("ip")

        if status == "ok" and server_ip:
            self.vm_ip = server_ip
            if self.client:
                try:
                    self.client.close()
                except Exception:
                    pass
            self.client = AgentClient(host=self.vm_ip)
        else:
            error_val = result.get("error")
            error_msg = str(error_val) if error_val is not None else "Unknown error"
            raise RuntimeError(f"Sandbox creation failed: {error_msg}")

    def create_by_snapshot(self):
        # Create sandbox from pre-defined snapshot
        url = f"{self.api_url}/api/sandbox/create"
        snap_config = config["snapshot"]
        payload = {
            "snapshot_id": self.snapshot_id,
            "image_id": "",
            "vmm_name": snap_config["vmm_name"],
            "sandbox_id": self.sandbox_id,
            "kernel_path": snap_config["kernel_path"],
            "disk_image_path": snap_config["disk_image_path"],
            "vcpu_num": snap_config["vcpu_num"],
            "ram_mb": snap_config["ram_mb"],
        }

        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result["sandbox_id"] = self.sandbox_id
            result["snapshot_id"] = self.snapshot_id
            print(f"Create Sandbox by Snapshot !")
            print(json.dumps(result, indent=4, ensure_ascii=False))
            return result
        except requests.exceptions.RequestException as e:
            error_msg = response.text if 'response' in locals() else str(e)
            print(f"Failed to create sandbox by snapshot !")
            return {
                "status": "failed",
                "sandbox_id": self.sandbox_id,
                "snapshot_id": self.snapshot_id,
                "error": error_msg
            }

    def create(self):
        # Create sandbox from base image
        url = f"{self.api_url}/api/sandbox/create"
        img_config = config["image"]
        payload = {
            "snapshot_id": "",
            "image_id": img_config["image_id"],
            "vmm_name": img_config["vmm_name"],
            "sandbox_id": self.sandbox_id,
            "kernel_path": img_config["kernel_path"],
            "disk_image_path": img_config["disk_image_path"],
            "vcpu_num": img_config["vcpu_num"],
            "ram_mb": img_config["ram_mb"],
        }

        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result["sandbox_id"] = self.sandbox_id
            print(f"Sandbox Created !")
            print(json.dumps(result, indent=4, ensure_ascii=False))
            return result
        except requests.exceptions.RequestException as e:
            error_msg = response.text if 'response' in locals() else str(e)
            print(f"Failed to create sandbox !")
            return {
                "status": "failed",
                "sandbox_id": self.sandbox_id,
                "error": error_msg
            }

    def delete(self):
        # Delete sandbox
        url = f"{self.api_url}/api/sandbox/delete"
        payload = {"sandbox_id": self.sandbox_id, }

        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result["sandbox_id"] = self.sandbox_id
            print(f"Sandbox Deleted !")
            print(json.dumps(result, indent=4, ensure_ascii=False))
        except requests.exceptions.RequestException as e:
            error_msg = response.text if 'response' in locals() else str(e)
            print(f"Failed to delete sandbox !")
            return {
                "status": "failed",
                "sandbox_id": self.sandbox_id,
                "error": error_msg
            }

    def pause(self):
        # Pause sandbox
        url = f"{self.api_url}/api/sandbox/pause"
        payload = {
            "sandbox_id": self.sandbox_id,
            "snapshot_id": self.snapshot_id,
        }

        try:
            response = requests.post(url, json=payload)
            response.raise_for_status()
            result = response.json()
            result["sandbox_id"] = self.sandbox_id
            print(f"Sandbox Paused !")
            print(json.dumps(result, indent=4, ensure_ascii=False))
        except requests.exceptions.RequestException as e:
            error_msg = response.text if 'response' in locals() else str(e)
            print(f"Failed to pause sandbox !")
            return {
                "status": "failed",
                "sandbox_id": self.sandbox_id,
                "error": error_msg
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
                "status": "ERROR",
                "message": f"Health check failed: {e}"
            }

    def upload(self, *args, **kwargs) -> Dict[str, Any]:
        # Upload files to sandbox working dir
        files = []
        if len(args) == 2:
            local_path, remote_path = args
            full_remote = f"{self.workdir}/{remote_path.lstrip('/')}"
            if not os.path.exists(local_path):
                return {"status": -1, "message": f"Local file not found: {local_path}"}
            if not os.path.isfile(local_path):
                return {"status": -1, "message": f"Not a file: {local_path}"}
            with open(local_path, "rb") as f:
                content = f.read()
            files.append({"filepath": full_remote, "content": content})
        elif len(args) == 1 and isinstance(args[0], (list, tuple)):
            file_specs = args[0]
            for item in file_specs:
                if not isinstance(item, dict) or "filepath" not in item or "content" not in item:
                    return {"status": -1, "message": f"Invalid file spec: {item}"}
                remote_path = item["filepath"]
                content = item["content"]
                full_remote = f"{self.workdir}/{remote_path.lstrip('/')}"
                files.append({"filepath": full_remote, "content": content})
        else:
            return {"status": -1, "message": "Invalid call. Usage: upload(local, remote) or upload([spec, ...])"}

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
                print(f"    stderr: {stderr}")
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
