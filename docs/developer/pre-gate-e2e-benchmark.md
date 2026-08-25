# StratoVirt Pre-gate End-to-end Benchmark

Date: 2026-08-25 (Asia/Shanghai)

## PR-ready summary

This benchmark compares the same StratoVirt checkpoint restore path with Conch
pre-gate enabled and disabled. End-to-end (E2E) latency is measured directly
from `template pull` start until every requested `sandbox create` returns
READY.

With one sandbox, pre-gate reduced mean pull-plus-start latency from 2.193 s to
1.920 s (**12.5%**). With 50 genuinely concurrent creates, pre-gate increased
mean latency from 3.475 s to 3.886 s (**11.8% regression**). All creates
succeeded: 5/5 in each single-sandbox mode and 250/250 in each concurrent mode.

| Scenario | Mode | Pull mean | Create wall mean | Direct E2E mean | E2E median | E2E range | Ready |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 sandbox | pre-gate on | 0.308 s | 1.581 s | **1.920 s** | 1.928 s | 1.799-2.064 s | 5/5 |
| 1 sandbox | pre-gate off | 1.656 s | 0.506 s | **2.193 s** | 2.185 s | 2.164-2.228 s | 5/5 |
| 50 concurrent | pre-gate on | 0.310 s | 3.351 s | **3.886 s** | 3.830 s | 3.726-4.080 s | 250/250 |
| 50 concurrent | pre-gate off | 1.720 s | 1.520 s | **3.475 s** | 3.541 s | 3.227-3.550 s | 250/250 |

Improvement is `(off - on) / off`; a negative value is a regression:

| Scenario | Mean delta | Mean improvement | Median improvement |
| --- | ---: | ---: | ---: |
| 1 sandbox | -0.274 s | **12.5%** | 11.8% |
| 50 concurrent | +0.411 s | **-11.8%** | -8.2% |

## Interpretation

Pre-gate consistently shortens `template pull` by deferring the 539 MB memory
layer. This is enough to improve the single-sandbox E2E path even though the
deferred work makes create slower.

At concurrency 50, all pre-gate restores share one full lazy materialization,
but every restore waits on its materialization/resume gate. The resulting
create batch takes 3.351 s on average, versus 1.520 s after the off path has
already transferred and unpacked the memory layer during pull. The 1.410 s
pull advantage therefore does not offset the 1.831 s create disadvantage.

The result supports the pre-gate mechanism for the measured single-instance
workflow, but it does **not** support claiming an E2E advantage at concurrency
50 in its current form. Improving the concurrent result requires shortening
the gated create critical path; benchmark-side leader prewarming would hide
that cost and is not representative of 50 concurrent client requests.

## Concurrency correctness

The 50-way test does not use a leader create and does not split startup into
`1 + 49`. After pull completes, 50 worker processes are created and stopped at
one barrier. The batch timer starts immediately before all 50 stopped workers
are continued. Each worker then independently issues its complete
`sandbox create` request.

Every valid concurrent round had these log counts:

| Event | Pre-gate on | Pre-gate off |
| --- | ---: | ---: |
| `pre-gate phase MaterializeCritical` | 50 | 0 |
| `pre-gate restore reached resume gate` | 50 | 0 |
| `Lazy memory layer materialized` | 1 | 0 |
| `Generated SnapshotID kind=mem-snapshot` | 0 | 1 |
| `Sandbox Agent is officially READY` | 50 | 50 |

The single off-path memory unpack is enforced by coalescing resolution of the
same Boot Index digest. The resolve cache and its original check-then-resolve
behavior were introduced by the pre-gate change; without coalescing, 50
simultaneous cache misses incorrectly performed the same 539 MB unpack 50
times. That behavior inflated the earlier off result and is not an inherent
cost of the pre-gate-off path.

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
| Conch baseline | `31fe983` plus the Boot Index resolve-coalescing fix |
| Conch daemon SHA-256 | `4ef84f6c403b8bcfd9b2777c117b912b264051ec4219886c62aee938fdb9ebf9` |
| StratoVirt baseline | `71a0c899` plus the current pre-gate working tree |
| StratoVirt build | `cargo build --release --features virtio_pmem,vhost_vsock` |
| StratoVirt binary SHA-256 | `9bf5b97d33a3fc159b360dbe8c363b8d03c5332f3701fb06e7379daf0ce16008` |
| Guest kernel SHA-256 | `9bcebdcf2035c6917e1a41cfcef0988d037553233a2620071871935f049ab7b1` |

The compared configurations are identical except for
`sandbox.stratovirt.pre_gate`. The network warm pool matches the requested
concurrency: one slot for the single-sandbox scenario and 50 slots for the
concurrent scenario.

## Method

Each valid sample uses this sequence:

1. Remove the mode-specific Conch work and state directories.
2. Start a new `conchd`; wait for the Unix API socket and complete network-pool
   prefill.
3. Read the registry memory blob once to `/dev/null` and verify its digest URL
   returns exactly 538,968,064 bytes. This happens before timing so local
   registry filesystem cache state is consistent between modes.
4. Start the E2E timer and run `conch template pull --plain-http`.
5. Create all requested worker processes and stop every worker at a barrier.
6. Start the create-batch timer and release all workers together. No create is
   issued before this release.
7. Stop both timers only after every create returns. A successful create
   returns after the sandbox agent reports READY.
8. Validate success and the expected log event counts, delete every sandbox,
   and stop the daemon.

No global Linux page-cache drop is used. The registry data and Conch state are
on the same filesystem, so `drop_caches` would also make the local registry
cold and would confound the pull comparison. Daemon startup, registry warming,
network-pool prefill, sandbox deletion, and shutdown are outside the E2E
interval. On/off execution order alternates by round.

Direct E2E is intentionally measured from one clock rather than calculated as
`pull wall + create wall`; it therefore includes the small cost of constructing
and synchronizing the worker batch after pull.

## Raw valid samples

### Single sandbox

| Mode | Run | Pull | Create wall | Direct E2E | Ready |
| --- | ---: | ---: | ---: | ---: | ---: |
| on | 1 | 0.298 s | 1.475 s | 1.799 s | 1/1 |
| on | 2 | 0.303 s | 1.729 s | 2.064 s | 1/1 |
| on | 3 | 0.324 s | 1.569 s | 1.928 s | 1/1 |
| on | 4 | 0.300 s | 1.510 s | 1.841 s | 1/1 |
| on | 5 | 0.317 s | 1.621 s | 1.966 s | 1/1 |
| off | 1 | 1.666 s | 0.488 s | 2.185 s | 1/1 |
| off | 2 | 1.680 s | 0.511 s | 2.228 s | 1/1 |
| off | 3 | 1.632 s | 0.503 s | 2.164 s | 1/1 |
| off | 4 | 1.653 s | 0.490 s | 2.172 s | 1/1 |
| off | 5 | 1.649 s | 0.539 s | 2.217 s | 1/1 |

### 50 concurrent sandboxes

| Mode | Run | Pull | Create wall | Direct E2E | Per-create p50 / p95 | Ready |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| on | 1 | 0.320 s | 3.514 s | 4.066 s | 3.487 / 3.500 s | 50/50 |
| on | 2 | 0.306 s | 3.215 s | 3.728 s | 2.928 / 3.058 s | 50/50 |
| on | 3 | 0.286 s | 3.339 s | 3.830 s | 3.308 / 3.321 s | 50/50 |
| on | 4 | 0.326 s | 3.528 s | 4.080 s | 3.502 / 3.514 s | 50/50 |
| on | 5 | 0.312 s | 3.159 s | 3.726 s | 2.807 / 2.972 s | 50/50 |
| off | 1 | 1.671 s | 1.344 s | 3.227 s | 1.251 / 1.325 s | 50/50 |
| off | 2 | 1.697 s | 1.571 s | 3.512 s | 1.501 / 1.554 s | 50/50 |
| off | 3 | 1.692 s | 1.621 s | 3.546 s | 1.540 / 1.602 s | 50/50 |
| off | 4 | 1.770 s | 1.528 s | 3.541 s | 1.441 / 1.511 s | 50/50 |
| off | 5 | 1.772 s | 1.538 s | 3.550 s | 1.483 / 1.518 s | 50/50 |

## Excluded setup attempts

Four daemon startup attempts were excluded before timing because CNI network
pool prefill stopped at 48 or 49 of 50 slots after netlink returned
`interrupted system call`. The benchmark retried with a fresh state directory;
no pull or create request had run, so these attempts contain no performance
sample. This is a separate CNI startup robustness issue.

## Limitations

- The registry is local and explicitly warmed. Remote bandwidth and latency
  can change the tradeoff substantially.
- Five samples show a consistent direction but do not establish a narrow
  confidence interval.
- The test covers one 512 MiB checkpoint and one host configuration.
- The current implementation waits for the full memory layer before opening
  the resume gate; it does not yet start the guest after fetching only the
  profiled pages.
- Daemon and network-pool startup are deliberately outside the measured
  pull-plus-start workflow.

## Conclusion

The corrected data proves a repeatable single-sandbox E2E benefit and validates
that one shared lazy materialization can serve 50 gated restores. It also shows
that the current gated critical path loses to pre-gate off under true 50-way
concurrency. The PR should claim the former and treat concurrent gate latency
as follow-up optimization work, not present the old 35.6% concurrent gain.
