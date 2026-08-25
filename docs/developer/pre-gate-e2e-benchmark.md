# StratoVirt Pre-gate End-to-end Benchmark

Date: 2026-08-25 (Asia/Shanghai)

## PR-ready summary

This benchmark compares the same StratoVirt checkpoint restore path with Conch
pre-gate enabled and disabled. End-to-end (E2E) latency is defined as:

```text
template pull start -> all requested sandbox create calls return READY
```

Using a cold Conch state directory and a local OCI registry for every sample,
pre-gate reduced mean E2E latency by **17.9% for one sandbox** and by **35.6%
for 50 concurrent sandboxes**. All measured creates succeeded: 5/5 in each
single-sandbox mode and 250/250 in each concurrent mode.

| Scenario | Mode | Pull mean | Create wall mean | E2E mean | E2E median | E2E range | Ready |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 sandbox | pre-gate on | 0.386 s | 1.666 s | **2.052 s** | 2.078 s | 1.975-2.105 s | 5/5 |
| 1 sandbox | pre-gate off | 1.950 s | 0.548 s | **2.499 s** | 2.528 s | 2.330-2.675 s | 5/5 |
| 50 concurrent | pre-gate on | 0.378 s | 4.575 s | **4.954 s** | 4.927 s | 4.864-5.119 s | 250/250 |
| 50 concurrent | pre-gate off | 1.946 s | 5.743 s | **7.689 s** | 7.510 s | 6.768-8.948 s | 250/250 |

Mean improvement is calculated as `(off - on) / off`:

| Scenario | Time saved | Mean improvement | Median improvement |
| --- | ---: | ---: | ---: |
| 1 sandbox | 0.447 s | **17.9%** | 17.8% |
| 50 concurrent | 2.735 s | **35.6%** | 34.4% |

## What the result demonstrates

Pre-gate moves the full memory-layer transfer out of `template pull` and into
the first restore while preserving restore correctness with a resume gate. In
the single-sandbox case, create is slower with pre-gate enabled, but the much
shorter pull produces a net E2E improvement.

The concurrent result is stronger. Every valid 50-sandbox round had these log
counts:

| Event | Pre-gate on | Pre-gate off |
| --- | ---: | ---: |
| `pre-gate phase MaterializeCritical` | 50 | 0 |
| `pre-gate restore reached resume gate` | 50 | 0 |
| `Lazy memory layer materialized` | 1 | 0 |
| `Generated SnapshotID kind=mem-snapshot` | 0 | 50 |
| `Sandbox Agent is officially READY` | 50 | 50 |

The 50 restores therefore shared one verified 539 MB full-layer fetch. The
non-pre-gate path pulled the complete memory content before create and then
showed more unpack/mount contention during concurrent create. This explains
why the E2E improvement increased from 17.9% at concurrency 1 to 35.6% at
concurrency 50.

The current implementation still waits for the complete memory layer before
opening the resume gate. The result proves that lazy OCI acquisition, shared
materialization, and deferred snapshot preparation improve the measured E2E
workflow; it does not claim that the guest starts after fetching only the
profiled pages.

## Test setup

| Item | Value |
| --- | --- |
| Host | 2 x Intel Xeon Gold 6430, 64 physical CPUs total |
| Memory | 251 GiB, no swap |
| Storage | ext4 on a local 1.7 TB disk; registry and Conch state on the same filesystem |
| Host kernel | Linux 6.18.1-061801-generic, x86_64 |
| OCI registry | `127.0.0.1:5000` (distribution registry) |
| OCI reference | `127.0.0.1:5000/conch/pre-gate:profiled` |
| OCI index digest | `sha256:89732894aaf15baf7bdda432900bf017436f1d89926666596d1c7eb2034dd52d` |
| Memory layer | 538,968,064 bytes |
| Portable pre-gate profile | 5,931 bytes |
| Restore-critical window | 45,211,648 bytes |
| Sandbox | 1 vCPU, 512 MiB RAM |
| Conch baseline | `9ecab24` plus the current pre-gate PR working tree |
| Conch daemon SHA-256 | `3c28507d20594ff435c776c47f489c6f2b67c5ec9cb87d4164be638853d6a2c0` |
| StratoVirt baseline | `71a0c899` plus the current pre-gate working tree |
| StratoVirt build | `cargo build --release --features virtio_pmem,vhost_vsock` |
| StratoVirt binary SHA-256 | `9bf5b97d33a3fc159b360dbe8c363b8d03c5332f3701fb06e7379daf0ce16008` |
| Guest kernel SHA-256 | `9bcebdcf2035c6917e1a41cfcef0988d037553233a2620071871935f049ab7b1` |

The compared configurations were identical except for
`sandbox.stratovirt.pre_gate`. The network warm pool matched the requested
concurrency: 1 slot for the single-sandbox scenario and 50 slots for the
concurrent scenario.

## Method

Each valid sample used this sequence:

1. Remove the mode-specific Conch work and state directories.
2. Start a new `conchd` and wait until its Unix API socket is ready.
3. Run `sync` and write `3` to `/proc/sys/vm/drop_caches`.
4. Time `conch template pull --plain-http` from the local OCI registry.
5. Immediately issue either one create or 50 parallel create calls.
6. Stop the create timer after every command returned successfully. A create
   command returns only after the sandbox agent reports READY.
7. Calculate `E2E = pull wall time + create batch wall time`.
8. Delete every sandbox, stop the daemon, and verify that no benchmark daemon,
   StratoVirt process, or benchmark mount remains.

There was no cache drop or artificial delay between pull and create. Daemon
startup, network-pool prefill, sandbox deletion, and daemon shutdown were not
included in E2E latency. The on/off execution order alternated between rounds
to reduce fixed ordering bias.

The essential measured commands were:

```bash
./bin/conch template pull \
  --config <mode-config> \
  --plain-http \
  127.0.0.1:5000/conch/pre-gate:profiled

./bin/conch sandbox create \
  --config <mode-config> \
  --template-id <pull-result-template-id> \
  --sandbox-id <unique-id>
```

For concurrency 50, the second command was launched 50 times in parallel with
unique sandbox IDs. The batch wall time ended when all 50 commands completed.

## Raw valid samples

### Single sandbox

| Mode | Run | Pull | Create wall | E2E | Ready |
| --- | ---: | ---: | ---: | ---: | ---: |
| on | 1 | 0.387 s | 1.692 s | 2.079 s | 1/1 |
| on | 2 | 0.385 s | 1.693 s | 2.078 s | 1/1 |
| on | 3 | 0.380 s | 1.642 s | 2.022 s | 1/1 |
| on | 4 | 0.396 s | 1.709 s | 2.105 s | 1/1 |
| on | 5 | 0.383 s | 1.592 s | 1.975 s | 1/1 |
| off | 1 | 1.834 s | 0.496 s | 2.330 s | 1/1 |
| off | 2 | 2.010 s | 0.578 s | 2.588 s | 1/1 |
| off | 3 | 1.856 s | 0.516 s | 2.372 s | 1/1 |
| off | 4 | 2.088 s | 0.587 s | 2.675 s | 1/1 |
| off | 5 | 1.963 s | 0.565 s | 2.528 s | 1/1 |

### 50 concurrent sandboxes

| Mode | Run | Pull | Create wall | E2E | Ready |
| --- | ---: | ---: | ---: | ---: | ---: |
| on | 1 | 0.383 s | 4.573 s | 4.956 s | 50/50 |
| on | 2 | 0.372 s | 4.530 s | 4.902 s | 50/50 |
| on | 3 | 0.388 s | 4.476 s | 4.864 s | 50/50 |
| on | 4 | 0.362 s | 4.565 s | 4.927 s | 50/50 |
| on | 5 | 0.387 s | 4.732 s | 5.119 s | 50/50 |
| off | 1 | 1.942 s | 5.568 s | 7.510 s | 50/50 |
| off | 2 | 1.898 s | 6.256 s | 8.154 s | 50/50 |
| off | 4 | 1.979 s | 6.969 s | 8.948 s | 50/50 |
| off | 5 | 1.917 s | 5.147 s | 7.064 s | 50/50 |
| off | 6 | 1.994 s | 4.774 s | 6.768 s | 50/50 |

## Invalid attempt and exclusion rule

One attempted off-mode concurrent round did not enter the timed pull/create
section. During daemon startup, CNI network-pool prefill failed at slot 49/50:

```text
initial prefill stopped before reaching target: current=49 target=50:
failed to set bridge addr: could not get list of IP addresses:
interrupted system call
```

This attempt contained no pull result and no sandbox create measurement. It
was excluded before performance analysis and replaced with run 6 so both modes
have five valid samples. The event is a CNI startup robustness issue, not a
pre-gate restore failure. It should be addressed separately by retrying the
transient interrupted operation.

## Limitations and follow-up work

- The registry was local. The result proves the optimization in this setup,
  but network latency and bandwidth should be varied before extrapolating to a
  production remote registry.
- Five samples establish a consistent direction but are not enough for a
  narrow confidence interval, especially for the more variable off-mode
  concurrent create time.
- Dropping the Linux page cache does not flush storage-device firmware caches.
- The test used one 512 MiB checkpoint and one host configuration.
- Daemon startup and network-pool creation were deliberately excluded because
  the measured workflow was template pull plus sandbox startup.
- Republishing a lazily pulled template is outside this benchmark. The current
  lazy materializer creates its verified EROFS backing and completion marker
  but does not repopulate the full memory blob in the containerd content store;
  a subsequent `template push` from that state fails until this path is fixed.

Recommended follow-up coverage is a bandwidth/latency matrix against a remote
registry, larger memory snapshots, and repeated concurrency levels such as 1,
10, 50, and 100.

## Conclusion

Within the stated scope, the data supports enabling the pre-gate path for the
pull-plus-start workflow. It produces a repeatable E2E reduction for one
sandbox and a larger reduction under 50-way concurrency, with no sandbox
readiness failures in the measured rounds. The concurrency logs also validate
the intended sharing behavior: one full memory-layer materialization serves
all 50 gated restores.
