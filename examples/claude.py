#!/usr/bin/env python3
#-*- coding: utf-8 -*-
import os
import sys
import subprocess
import time
import ipaddress

from conch import Sandbox

# Configuration:
#   Option 1: Set environment variables (recommended):
#     export ANTHROPIC_API_KEY="your_key"
#     export ANTHROPIC_BASE_URL="your_url"
#     export CLAUDE_SSH_AUTHORIZED_KEYS="$(cat ~/.ssh/id_rsa.pub)"
#     export CONCH_TEMPLATE_NAME="<template-name>"
#   Option 2: Enter values interactively when prompted

def get_config(name, prompt, default=None, required=True):
    value = os.environ.get(name)
    if value:
        return value
    if default is not None:
        return default
    if required:
        return input(f"{prompt}: ")
    return None

env = {
    "ANTHROPIC_API_KEY": get_config("ANTHROPIC_API_KEY", "Enter Anthropic API Key"),
    "ANTHROPIC_BASE_URL": get_config("ANTHROPIC_BASE_URL", "Enter API Base URL"),
    "ANTHROPIC_MODEL": "GLM-4.7",  # Change model if needed
}

api_key = env["ANTHROPIC_API_KEY"]
base_url = env["ANTHROPIC_BASE_URL"]

# Claude settings configuration (customize models if needed)
claude_settings = f'''{{
    "env": {{
        "ANTHROPIC_AUTH_TOKEN": "{api_key}",
        "ANTHROPIC_BASE_URL": "{base_url}",
        "API_TIMEOUT_MS": "3000000",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "ANTHROPIC_DEFAULT_OPUS_MODEL": "glm-5",
        "ANTHROPIC_DEFAULT_SONNET_MODEL": "glm-4.7",
        "ANTHROPIC_DEFAULT_HAIKU_MODEL": "glm-4.5-air"
    }},
    "skipDangerousModePermissionPrompt": true
}}'''

rsa_pub = get_config("CLAUDE_SSH_AUTHORIZED_KEYS", "Enter SSH public key")
template_name = get_config("CONCH_TEMPLATE_NAME", "Enter Conch Template Name")

def add_config(box):
    # 1. create .claude folder and settings.json
    result = box.commands.run(
        "sh",
        args=[
            "-c",
            f"mkdir -p /root/.claude && cat > /root/.claude/settings.json << 'EOF'\n{claude_settings}\nEOF"
        ],
    )
    
    # 2. Configure the SSH authorized_keys file
    result = box.commands.run(
        "sh",
        args=[
            "-c",
            f"mkdir -p /root/.ssh && cat > /root/.ssh/authorized_keys << 'EOF'\n{rsa_pub}\nEOF && chmod 600 /root/.ssh/authorized_keys"
        ],
    )

def prepare_box(template_name):
    box = Sandbox.create(template_name=template_name)
    print(f'sandbox {box.sandbox_id} created')
    ip = box.ip
    try:
        add_config(box)
    except Exception:
        box.delete()
        raise
    print(f'sandbox prepared ok with ip={ip}')
    return box, ip

def run_claude_once(box, ip):
    result = box.commands.run(
        "claude",
        args=["-p", "What's the day today?",],
        env=env,
    )
    print(f'claude stdout: {result.stdout}')
    print(f'claude stderr: {result.stderr}')

def run_claude_tui(box, ip):
    address = str(ipaddress.IPv4Address(ip))
    subprocess.run([
        "ssh",
        "-t",
        "-o",
        "StrictHostKeyChecking=no",
        "-o",
        "UserKnownHostsFile=/dev/null",
        f"root@{address}",
    ], check=True)

def main():
    if __name__ != '__main__':
        return
    box = None
    try:
        box, ip = prepare_box(template_name)
        run_claude_once(box, ip)
        run_claude_tui(box, ip)
    except Exception as e:
        print(e)
    finally:
        if box:
            box.delete()

main()
