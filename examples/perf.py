#!/usr/bin/env python3
# -*- coding: utf-8 -*-
import time

from conch import Sandbox


def perf_print_hello_by_image():
    box = None
    paused = False
    t0 = time.perf_counter()
    try:
        box = Sandbox.create()
        t1 = time.perf_counter()

        ret = box.execute(cmd='python3', content='print("hello")')
        t2 = time.perf_counter()

        if ret.exit_code != 0:
            print(f"Execute failed: {ret.stderr}")
            return None

        startup_s = t1 - t0
        exec_s = t2 - t1
        total_s = t2 - t0

        print(f"--- IMAGE (Cold Start) ---")
        print(f"Startup Cost:   {startup_s:.3f}s ({startup_s*1000:.2f}ms)")
        print(f"Execution Only: {exec_s:.3f}s ({exec_s*1000:.2f}ms)")
        print(f"Total Workflow: {total_s:.3f}s ({total_s*1000:.2f}ms)")

        snapshot = box.pause()
        paused = True
        print(f"Snapshot {snapshot.snapshot_id} created\n")
        return snapshot.snapshot_id

    except Exception as e:
        print(f"Image Error: {e}")
        return None
    finally:
        if box and not paused:
            sid = box.sandbox_id
            try:
                box.delete()
                print(f"Sandbox {sid} cleaned up.")
            except Exception as e:
                print(f"Warning: Failed to delete sandbox: {e}")

def perf_print_hello_by_snapshot(snapshot_id):
    if not snapshot_id:
        return

    box = None
    t0 = time.perf_counter()
    try:
        box = Sandbox.create(snapshot_id)
        t1 = time.perf_counter()

        ret = box.execute(cmd='python3', content='print("hello")')
        t2 = time.perf_counter()

        if ret.exit_code != 0:
            print(f"Execute failed: {ret.stderr}")
            return

        restore_s = t1 - t0
        exec_s = t2 - t1
        total_s = t2 - t0

        print(f"--- SNAPSHOT (Hot Start) ---")
        print(f"Restore Cost:   {restore_s:.3f}s ({restore_s*1000:.2f}ms)")
        print(f"Execution Only: {exec_s:.3f}s ({exec_s*1000:.2f}ms)")
        print(f"Total Workflow: {total_s:.3f}s ({total_s*1000:.2f}ms)")

    except Exception as e:
        print(f"Snapshot Error: {e}")
    finally:
        if box:
            sid = box.sandbox_id
            try:
                box.delete()
                print(f"Sandbox {sid} cleaned up.")
            except Exception as e:
                print(f"Warning: Failed to delete sandbox: {e}")

def main():
    snapshot_id = perf_print_hello_by_image()
    if snapshot_id:
        perf_print_hello_by_snapshot(snapshot_id)
    else:
        print("Test terminated due to Image Flow error.")


if __name__ == "__main__":
    main()
