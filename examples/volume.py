#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Volume functionality test for Conch (single-virtiofsd, host-path model).

Verifies that a host directory mounted into a sandbox persists across sandbox
lifecycles when the same host path is reused:

  1. Ensure host volume dirs exist under ./shared (volume-1 .. volume-10).
  2. Start a sandbox with ./shared/volume-1 mounted at /workspace, write the
     current timestamp to /workspace/last.txt, then delete the sandbox.
  3. Start a second sandbox with the SAME host dir mounted at /workspace,
     read /workspace/last.txt back to verify data persisted, then delete.

Host dirs are created under PWD and left in place for inspection (no tempfile
auto-cleanup), so the persisted last.txt is visible after the run.

Run: python3 tests/volume.py <template_name>
"""

import os
import platform
import time
import sys
from conch import Sandbox


HOST_BASE = os.path.join(os.getcwd(), "debug/shared")
VOLUME_NAMES = [f"volume-{i}" for i in range(1, 11)]  # volume-1 .. volume-10
MOUNT_PATH = "/workspace"

# Sandbox specification
IMAGE_NAME = f"conch/openeuler:volume-{platform.machine()}"
VCPU_NUM = 2
VCPU_MAX = 2
RAM_MB = 4096

def print_cost(key, start):
    cost = time.perf_counter() - start
    print(f'{key} cost: {cost:.3f}s')


def ensure_host_volumes():
    os.makedirs(HOST_BASE, exist_ok=True)
    for name in VOLUME_NAMES:
        os.makedirs(os.path.join(HOST_BASE, name), exist_ok=True)


def print_exec(title, ret):
    print(f"--- {title} ---")
    print(f"exit_code: {ret.exit_code}")
    if ret.stdout.strip():
        print(f"stdout:\n{ret.stdout.strip()}")
    if ret.stderr.strip():
        print(f"stderr:\n{ret.stderr.strip()}")


def run_sandbox_write(tid):
    t0 = time.perf_counter()

    box = Sandbox.create(
        template_name=tid,
        vcpu_num=VCPU_NUM,
        vcpu_max=VCPU_MAX,
        ram_mb=RAM_MB,
        volume_mounts=[{"source": os.path.join(HOST_BASE, "volume-1"), "path": MOUNT_PATH}],
    )
    sid = box.sandbox_id
    try:
        ret = box.commands.run(cmd="sh", args=["-c", "df -hT"])
        print_exec("df -hT", ret)
        print_cost(f'cold-start {VCPU_NUM}-CPU {RAM_MB}MB df -hT', t0)
        ret = box.commands.run(cmd="sh", args=["-c", f"echo $(date) > {MOUNT_PATH}/last.txt"])
        print_exec(f"echo $(date) > {MOUNT_PATH}/last.txt", ret)
    finally:
        try:
            box.delete()
            print(f"Sandbox {sid} deleted.")
        except Exception as e:
            print(f"Warning: Failed to delete sandbox {sid}: {e}")


def run_sandbox_read(tid):
    box = Sandbox.create(
        template_name=tid,
        vcpu_num=VCPU_NUM,
        vcpu_max=VCPU_MAX,
        ram_mb=RAM_MB,
        volume_mounts=[{"source": os.path.join(HOST_BASE, "volume-1"), "path": MOUNT_PATH}],
    )
    sid = box.sandbox_id
    try:
        ret = box.commands.run(cmd="cat", args=[f"{MOUNT_PATH}/last.txt"])
        print_exec(f"cat {MOUNT_PATH}/last.txt", ret)
    finally:
        try:
            box.delete()
            print(f"Sandbox {sid} deleted.")
        except Exception as e:
            print(f"Warning: Failed to delete sandbox {sid}: {e}")


def main():
    ensure_host_volumes()
    print(f"Host volume base: {HOST_BASE}")

    if len(sys.argv) < 2:
        print('missing template_name')
        return
    tid = sys.argv[1]
    print(f'using template_name: {tid}')

    print("\n=== First sandbox: write to volume ===")
    run_sandbox_write(tid)

    print("\n=== Second sandbox: read from volume ===")
    run_sandbox_read(tid)


if __name__ == "__main__":
    main()
