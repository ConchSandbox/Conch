#!/usr/bin/env python3
# -*- coding: utf-8 -*-
import time

from conch import Sandbox


def perf_print_hello_by_template():
    box = None
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

        print("--- TEMPLATE (Cold Start) ---")
        print(f"Startup Cost:   {startup_s:.3f}s ({startup_s*1000:.2f}ms)")
        print(f"Execution Only: {exec_s:.3f}s ({exec_s*1000:.2f}ms)")
        print(f"Total Workflow: {total_s:.3f}s ({total_s*1000:.2f}ms)")

        template = box.checkpoint()
        print(f"Checkpoint template {template.template_id} created\n")
        return template.template_id

    except Exception as e:
        print(f"Template Error: {e}")
        return None
    finally:
        if box:
            sid = box.sandbox_id
            try:
                box.delete()
                print(f"Sandbox {sid} cleaned up.")
            except Exception as e:
                print(f"Warning: Failed to delete sandbox: {e}")

def perf_print_hello_by_checkpoint(template_id):
    if not template_id:
        return

    box = None
    t0 = time.perf_counter()
    try:
        box = Sandbox.create(template_id=template_id)
        t1 = time.perf_counter()

        ret = box.execute(cmd='python3', content='print("hello")')
        t2 = time.perf_counter()

        if ret.exit_code != 0:
            print(f"Execute failed: {ret.stderr}")
            return

        restore_s = t1 - t0
        exec_s = t2 - t1
        total_s = t2 - t0

        print("--- CHECKPOINT TEMPLATE (Resume) ---")
        print(f"Restore Cost:   {restore_s:.3f}s ({restore_s*1000:.2f}ms)")
        print(f"Execution Only: {exec_s:.3f}s ({exec_s*1000:.2f}ms)")
        print(f"Total Workflow: {total_s:.3f}s ({total_s*1000:.2f}ms)")

    except Exception as e:
        print(f"Checkpoint Template Error: {e}")
    finally:
        if box:
            sid = box.sandbox_id
            try:
                box.delete()
                print(f"Sandbox {sid} cleaned up.")
            except Exception as e:
                print(f"Warning: Failed to delete sandbox: {e}")

def main():
    template_id = perf_print_hello_by_template()
    if template_id:
        perf_print_hello_by_checkpoint(template_id)
    else:
        print("Test terminated due to template flow error.")


if __name__ == "__main__":
    main()
