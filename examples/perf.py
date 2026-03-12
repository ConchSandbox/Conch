#!/usr/bin/env python3
#-*- coding: utf-8 -*-
import os
import sys
import subprocess
import time

from conch import Sandbox

def perf_print_hello():
    t0 = time.perf_counter()
    box = Sandbox()
    err = box.create()
    if err:
        print(f'create {box.sandbox_id} failed: {err}')
        return
    else:
        print(f'sandbox {box.sandbox_id} created')

    ret = box.execute(cmd='python3',
                      content='print("hello")')
    t1 = time.perf_counter()
    cost = t1 - t0
    print(f'print hello cost {cost:.3f}s')

    err = box.delete()
    if err:
        print(f'delete {box.sandbox_id} failed: {err}')
        return
    else:
        print(f'delete {box.sandbox_id} ok')

def main():
    if __name__ != '__main__':
        return

    perf_print_hello()

main()


