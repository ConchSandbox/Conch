#!/usr/bin/env python3
# -*- coding: utf-8 -*-
import os
import time

from conch import CommandExitException, Sandbox


def perf_print_hello_by_template(template_name):
    box = None
    t0 = time.perf_counter()
    try:
        box = Sandbox.create(template_name=template_name)
        t1 = time.perf_counter()

        try:
            box.commands.run(cmd='python3', content='print("hello")')
        except CommandExitException as e:
            print(f"Execute failed: {e.stderr or e.error}")
            return None
        t2 = time.perf_counter()

        startup_s = t1 - t0
        exec_s = t2 - t1
        total_s = t2 - t0

        print("--- TEMPLATE (Cold Start) ---")
        print(f"Startup Cost:   {startup_s:.3f}s ({startup_s*1000:.2f}ms)")
        print(f"Execution Only: {exec_s:.3f}s ({exec_s*1000:.2f}ms)")
        print(f"Total Workflow: {total_s:.3f}s ({total_s*1000:.2f}ms)")

        checkpoint_name = f"localhost/conch/perf-checkpoint-{box.sandbox_id}:latest"
        template = box.checkpoint(checkpoint_name)
        print(f"Checkpoint template {template.template_name} -> {template.template_id} created\n")
        return template.template_name

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

def perf_print_hello_by_checkpoint(template_name):
    if not template_name:
        return

    box = None
    t0 = time.perf_counter()
    try:
        box = Sandbox.create(template_name=template_name)
        t1 = time.perf_counter()

        try:
            box.commands.run(cmd='python3', content='print("hello")')
        except CommandExitException as e:
            print(f"Execute failed: {e.stderr or e.error}")
            return
        t2 = time.perf_counter()

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
    source_template_name = os.environ.get("CONCH_TEMPLATE_NAME")
    if not source_template_name:
        print("CONCH_TEMPLATE_NAME is required")
        return

    checkpoint_template_name = perf_print_hello_by_template(source_template_name)
    if checkpoint_template_name:
        perf_print_hello_by_checkpoint(checkpoint_template_name)
    else:
        print("Test terminated due to template flow error.")


if __name__ == "__main__":
    main()
