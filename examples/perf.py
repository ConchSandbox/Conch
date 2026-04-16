#!/usr/bin/env python3
#-*- coding: utf-8 -*-
import os
import sys
import time

from conch import Sandbox

def perf_print_hello_by_image():
    t0 = time.perf_counter()
    try:
        box = Sandbox.create()
        print(f'sandbox {box.sandbox_id} created')

        # 模拟执行并获取输出，直到 EOF
        ret = box.execute(cmd='python3', content='import os; print(1+1)')

        # 检查输出是否在 EOF 前匹配预期结果
        output = ret.stdout.strip()
        if output != "2":
            print(f"Calculation failed at EOF, output: {output}")
            return None

        t1 = time.perf_counter()
        print(f'image create print hello cost {t1 - t0:.3f}s')

        snapshot = box.pause()
        if snapshot is None:
            raise RuntimeError("pause failed, no snapshot created")

        print(f'snapshot {snapshot.snapshot_id} created, reaching EOF of image setup')
        return snapshot.snapshot_id

    except RuntimeError as e:
        print(f'Error: {e}')
        return None

def perf_print_hello_by_snapshot(snapshot_id):
    if not snapshot_id:
        return

    t0 = time.perf_counter()
    try:
        box = Sandbox.create(snapshot_id)
        ret = box.execute(cmd='python3', content='import os; print(1+1)')

        # 验证结果
        if ret.stdout.strip() == "2":
            t1 = time.perf_counter()
            print(f'snapshot create print hello cost {t1 - t0:.3f}s')
            print("Process reached EOF successfully.")

        box.delete()
    except RuntimeError as e:
        print(f'Error: {e}')

def main():
    snapshot_id = perf_print_hello_by_image()
    if snapshot_id:
        perf_print_hello_by_snapshot(snapshot_id)
    else:
        print("Process terminated before EOF.")

if __name__ == '__main__':
    main()
