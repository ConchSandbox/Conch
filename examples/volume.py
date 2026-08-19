#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Volume functionality test for Conch (single-virtiofsd, host-path model).

Verifies that a host directory mounted into a sandbox persists across sandbox
lifecycles when the same host path is reused:

  1. Ensure host volume dirs exist under ./debug/shared (volume-1 .. volume-100).
  2. Start a sandbox with N host volumes mounted under /workspace, writing the
     current timestamp to each mount point.
  3. Start a second sandbox with the SAME host dirs mounted at the same paths,
     read each timestamp back, and print it.
  4. Wait for confirmation, then delete both sandboxes.

Host dirs are created under PWD and left in place for inspection (no tempfile
auto-cleanup), so the persisted last.txt is visible after the run.

Run: CONCH_TEMPLATE_ID=<template_id> python3 examples/volume.py
"""

import argparse
import os
import time
import sys
from conch import Sandbox


HOST_BASE = os.path.join(os.getcwd(), "debug/shared")
VOLUME_NAMES = [f"volume-{i}" for i in range(1, 100)]  # volume-1 .. volume-100
MOUNT_PATH = "/workspace"
MAX_MOUNTS = len(VOLUME_NAMES)

# Sandbox specification
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


def mount_points(count):
    return [MOUNT_PATH if i == 1 else f"{MOUNT_PATH}-{i}" for i in range(1, count + 1)]


def volume_mounts(count):
    return [
        {"source": os.path.join(HOST_BASE, f"volume-{i}"), "path": path}
        for i, path in enumerate(mount_points(count), start=1)
    ]


def run_sandbox_write(tid, count):
    t0 = time.perf_counter()

    box = Sandbox.create(
        template_id=tid,
        vcpu_num=VCPU_NUM,
        vcpu_max=VCPU_MAX,
        ram_mb=RAM_MB,
        volume_mounts=volume_mounts(count),
    )
    ret = box.commands.run(cmd="sh", args=["-c", "df -hT"])
    print_exec("df -hT", ret)
    print_cost(f'cold-start {VCPU_NUM}-CPU {RAM_MB}MB df -hT', t0)
    for path in mount_points(count):
        ret = box.commands.run(cmd="sh", args=["-c", f"date > {path}/last.txt"])
        print_exec(f"date > {path}/last.txt", ret)
    return box


def run_sandbox_read(tid, count):
    box = Sandbox.create(
        template_id=tid,
        vcpu_num=VCPU_NUM,
        vcpu_max=VCPU_MAX,
        ram_mb=RAM_MB,
        volume_mounts=volume_mounts(count),
    )
    for path in mount_points(count):
        ret = box.commands.run(cmd="cat", args=[f"{path}/last.txt"])
        date = ret.stdout.strip() or ret.stderr.strip() or "<unavailable>"
        print(f"{path}: {date}")
    return box


def delete_sandboxes(sandboxes):
    for box in reversed(sandboxes):
        sid = box.sandbox_id
        try:
            box.delete()
            print(f"Sandbox {sid} deleted.")
        except Exception as e:
            print(f"Warning: Failed to delete sandbox {sid}: {e}")


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog="volume.py",
        description=(
            "Volume functionality test for Conch (single-virtiofsd, host-path model). "
            "Mounts a host dir into a sandbox, writes a timestamp, then verifies the "
            "data persists across a second sandbox reusing the same host path."
        ),
        epilog=(
            "Host dirs are created under PWD/debug/shared and left in place for "
            "inspection. Set CONCH_TEMPLATE_ID before running, for example: "
            "CONCH_TEMPLATE_ID=sha256:abcd1234... python3 examples/volume.py"
        ),
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument(
        "-n",
        type=int,
        default=1,
        metavar="N",
        help=f"number of volume mount points (1-{MAX_MOUNTS}, default: 1)",
    )
    return parser.parse_args(argv)


def main():
    args = parse_args(sys.argv[1:])
    template_id = os.environ.get("CONCH_TEMPLATE_ID")
    if not template_id:
        print("CONCH_TEMPLATE_ID is required")
        return
    if not 1 <= args.n <= MAX_MOUNTS:
        raise SystemExit(f"-n must be between 1 and {MAX_MOUNTS}")
    ensure_host_volumes()
    print(f"Host volume base: {HOST_BASE}")
    print(f"using template_id: {template_id}")

    sandboxes = []
    try:
        print("\n=== First sandbox: write to volume ===")
        sandboxes.append(run_sandbox_write(template_id, args.n))

        print("\n=== Second sandbox: read from volume ===")
        sandboxes.append(run_sandbox_read(template_id, args.n))
    finally:
        if sandboxes:
            delete_sandboxes(sandboxes)


if __name__ == "__main__":
    main()
