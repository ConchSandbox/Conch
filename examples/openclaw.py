#!/usr/bin/env python3
# -*- coding: utf-8 -*-

import os
import json
import getpass
from conch import Sandbox

def get_config(env_name, prompt, default=None, is_secret=False):
    """
    Retrieves configuration and strips terminal bracketed paste sequences.
    Explicitly labels the default value for clarity.
    """
    value = os.environ.get(env_name)
    if value:
        return value
    
    # Format prompt to clearly show the default option
    display_prompt = prompt.strip()
    if default:
        display_prompt = f"{display_prompt} (default: {default}): "
    else:
        display_prompt = f"{display_prompt}: "
    
    raw_val = getpass.getpass(display_prompt) if is_secret else input(display_prompt)
    
    # Use default value if the user provides no input
    if not raw_val and default:
        return default

    # Clean up bracketed paste mode sequences (^[[200~ and ^[[201~) common in Xshell
    clean_val = raw_val.replace('\x1b[200~', '').replace('\x1b[201~', '')
    
    return clean_val.strip()

def get_sandbox_home(sandbox):
    """
    Get actual home directory in sandbox.
    Dynamically adapt to different users.
    """
    whoami_result = sandbox.execute(cmd="whoami")
    username = whoami_result.stdout.strip()
    
    if username == 'root':
        return '/root'
    
    grep_result = sandbox.execute(
        cmd="sh",
        args=["-c", f"grep '^{username}:' /etc/passwd"]
    )
    
    fields = grep_result.stdout.strip().split(':')
    return fields[5] if len(fields) >= 6 else '/'

def setup_sandbox_configs(sandbox):
    """
    Write required OpenClaw configurations to the sandbox filesystem.
    """
    # Initialize network interface
    sandbox.execute(cmd="ip", args=["link", "set", "lo", "up"])

    # Get sandbox home directory (adapt to different users)
    sandbox_home = get_sandbox_home(sandbox)

    # Collect configuration inputs
    api_key = get_config("OPENCLAW_API_KEY", "Enter API Key", is_secret=True)
    base_url = get_config("OPENCLAW_BASE_URL", "Enter Base URL")
    model_name = get_config("OPENCLAW_MODEL_NAME", "Enter Model Name", default="MiniMax-M2.5")

    # Define model provider configuration
    openclaw_config = {
        "models": {
            "mode": "merge",
            "providers": {
                "vllm-dev": {
                    "baseUrl": base_url,
                    "apiKey": api_key,
                    "api": "openai-completions",
                    "models": [{
                        "id": model_name,
                        "name": model_name,
                        "reasoning": True,
                        "contextWindow": 200000
                    }]
                }
            }
        }
    }

    # Define default agent configuration
    agent_config = {
        "agents": {
            "defaults": {
                "model": {
                    "primary": f"vllm-dev/{model_name}"
                }
            }
        }
    }

    # Combine configurations
    full_config = {**agent_config, **openclaw_config}

    file_mappings = [
        (f"{sandbox_home}/.openclaw/auth.json", {"token": "auto"}),
        (f"{sandbox_home}/.config/openclaw/config.json", {"gateway": {"mode": "local"}}),
        (f"{sandbox_home}/.openclaw/openclaw.json", full_config)
    ]

    # Upload files directly
    files_to_upload = []
    for file_path, content in file_mappings:
        json_str = json.dumps(content, indent=2)
        files_to_upload.append({
            "filepath": file_path,
            "content": json_str.encode()
        })
    sandbox.upload(files_to_upload)

def main():
    template_id = get_config("CONCH_TEMPLATE_ID", "Enter Conch Template ID")
    sandbox = Sandbox.create(template_id=template_id)
    print(f"Sandbox created: {sandbox.sandbox_id} (IP: {sandbox.ip})")

    try:
        setup_sandbox_configs(sandbox)

        # Get sandbox user for SSH connection
        whoami_result = sandbox.execute(cmd="whoami")
        sandbox_user = whoami_result.stdout.strip()

        print("Configuration complete, ready to start TUI...\n")

        ssh_cmd = (
            f"ssh -t "
            f"-o UserKnownHostsFile=/dev/null "
            f"-o StrictHostKeyChecking=no "
            f"{sandbox_user}@{sandbox.ip} "
            f"\"openclaw gateway --allow-unconfigured > /dev/null 2>&1 & sleep 2 && openclaw tui; bash -l\""
        )

        os.system(ssh_cmd)

    finally:
        sandbox.delete()
        print("Sandbox destroyed.")

if __name__ == "__main__":
    main()
