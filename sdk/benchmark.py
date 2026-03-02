import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from conch import Sandbox

def run_sandbox_task(index, snapshot_id):
    sbx = Sandbox(use_snapshot=True, snapshot_id=snapshot_id)
    sbx.execute(cmd="expr 1 + 1")
    return

def main():
    try:
        n = int(input("Enter concurrency count n: "))
        sandbox_id = input("Enter snapshot_id: ")
    except ValueError:
        print("Invalid input, please enter a number.")
        return

    print(f"\nStarting {n} Sandboxes concurrently...\n")

    t1_start = time.perf_counter()

    with ThreadPoolExecutor(max_workers=n) as executor:
        futures = {executor.submit(run_sandbox_task, i, sandbox_id): i for i in range(1, n + 1)}
        
        for future in as_completed(futures):
            future.result()

    t1_end = time.perf_counter()
    total_duration = t1_end - t1_start

    print("-" * 30)
    print(f"All tasks completed!")
    print(f"Current time: {time.strftime('%Y-%m-%d %H:%M:%S', time.localtime())}")
    print(f"Launched {n} Sandboxes concurrently, total elapsed: {total_duration:.4f} seconds")

if __name__ == "__main__":
    main()
