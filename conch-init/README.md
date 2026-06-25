# conch-init

`conch-init` is the guest-side init and control service for Conch sandboxes. It is built as a Rust musl binary, packaged into the sandbox initramfs, and launched as PID 1 inside the guest.

Unlike the host-side `conchd` and runtime managers, `conch-init` runs inside the sandbox boundary. It prepares the guest environment, starts the gRPC control service, handles the sandbox-ready handshake over vsock, and executes process/file operations requested by the host SDK or runtime.

## Role in the System

```text
Host
┌─────────────────────────────────────────────┐
│  conchd / SDK / runtime manager             │
│       │                                     │
│       │  gRPC over sandbox network :4064    │
│       │  vsock ready handshake :4065        │
└───────┼─────────────────────────────────────┘
        │
   Sandbox guest boundary
        │
┌───────▼─────────────────────────────────────┐
│  conch-init  (PID 1 / /init)                │
│       │                                     │
│       ├─ mount proc/sys/dev/tmp/overlay     │
│       ├─ setup guest network                │
│       ├─ start AgentService gRPC server     │
│       ├─ receive sandbox token over vsock   │
│       └─ execute commands and file streams  │
└─────────────────────────────────────────────┘
```

When a sandbox boots, `conch-init` starts immediately and:

1. **Initializes the guest environment** - mounts essential filesystems, prepares `/dev/null`, mounts storage devices, prepares OverlayFS under `/mnt/conch/merge`, and configures networking.
2. **Starts the control plane** - exposes `pb.AgentService` over gRPC on `0.0.0.0:4064`.
3. **Handles readiness over vsock** - listens on vsock port `4065`, receives `SANDBOX_ID` and `AGENT_TOKEN`, initializes request authentication, and returns `READY:<version>` after the guest is healthy.
4. **Runs rootfs entrypoint hooks** - if `/mnt/conch/merge/etc/conch/entrypoint` exists and is executable, starts it inside the merged rootfs and waits for that entrypoint to signal readiness. If the entrypoint does not exist, rootfs service readiness is not required.
5. **Executes workload commands** - supports command execution, inline script content, cwd/env propagation, stdout/stderr capture, and exit-code reporting.
6. **Transfers files** - supports streamed upload and download through gRPC.
7. **Reaps child processes** - runs as PID 1 and reaps exited child processes.

## Repository Layout

```text
conch-init/
├── Cargo.toml          # Rust package metadata and dependencies
├── Makefile            # musl build/check/test/install targets
├── build.rs            # tonic protobuf binding generation
└── src/
    ├── main.rs         # CLI mode, PID 1 detection, server startup
    ├── init.rs         # PID 1 boot sequence
    ├── grpc.rs         # AgentService implementation
    ├── vsock.rs        # sandbox-ready handshake
    ├── auth.rs         # gRPC token authentication
    ├── state.rs        # shared sandbox readiness/auth state
    ├── mount.rs        # guest filesystem and storage mounts
    ├── network.rs      # guest network setup
    ├── devpts.rs       # devpts setup under merged rootfs
    ├── rootfs_entrypoint.rs
    ├── reaper.rs       # PID 1 child reaping
    ├── logging.rs
    ├── util.rs
    └── pb.rs           # generated protobuf module include
```

Related files:

```text
api/agent.proto                         # gRPC protocol definition
scripts/build-conch-init-initramfs.sh   # initramfs builder
Makefile                                # top-level build targets
examples/e2b-rootfs/conch-entrypoint.sh # optional rootfs service readiness hook
```

## Boot Flow

`conch-init` is intended to run as the sandbox guest init process. The Rust entry point always executes the init flow; `--init` is accepted only as a compatibility flag.

The init flow performs the following sequence:

1. Set `PATH` to `/sbin:/bin:/usr/sbin:/usr/bin`.
2. Mount `/proc` if needed.
3. Read `conch.sandbox_id` from kernel cmdline when present.
4. Create `/dev/null`.
5. Mount essential filesystems.
6. Mount storage devices and prepare the merged rootfs.
7. Configure guest networking.
8. Start rootfs entrypoint if present.
9. `chroot` into `/mnt/conch/merge` when the overlay rootfs is ready.
10. Start the gRPC server on port `4064`.
11. Start the vsock readiness listener on port `4065`.
12. Start the child reaper and wait for shutdown signals.

## Readiness Handshake

The host initializes the sandbox by connecting to vsock port `4065` and sending a message containing:

```text
I AM SANDBOX_ID:<sandbox-id>
AGENT_TOKEN:<token>
```

`conch-init` stores the token for subsequent gRPC authentication and responds:

```text
OK
READY:0.0.4
```

If gRPC is not ready, or an existing rootfs entrypoint has not reported readiness before the readiness timeout, it responds:

```text
NOT_READY
```

The current expected version is defined by `SERVER_VERSION` in `conch-init/src/constants.rs`.

## gRPC API

`conch-init` implements `pb.AgentService` from `api/agent.proto`.

| RPC | Purpose |
| --- | --- |
| `HealthCheck(Empty)` | Return `message: "OK"` when the service is reachable. |
| `StartProcess(StartProcessRequest)` | Run a command inside the guest and return stdout, stderr, exit code, and error text. |
| `PostFileStream(stream FileChunk)` | Upload one file through a client-streaming RPC. |
| `GetFileStream(GetFileRequest)` | Download one file through a server-streaming RPC. |

All normal gRPC requests require metadata:

```text
x-conch-agent-token: <token>
```

The metadata name intentionally remains `x-conch-agent-token` for protocol compatibility.

### StartProcess

`StartProcess` supports:

- `cmd`: executable name or path.
- `args`: command arguments.
- `cwd`: working directory; created if missing.
- `env`: environment variables.
- `content`: inline script body.

When `content` is set and `args` is empty, `conch-init` writes the content to a temporary file in `cwd` and passes that file as the only argument to `cmd`. Script suffix is inferred from the command, for example `.py` for `python3` and `.sh` for `sh`.

Example:

```json
{
  "cmd": "python3",
  "cwd": "/tmp",
  "env": {
    "CONCH_VERIFY_ENV": "ok"
  },
  "content": "import os\nprint(os.environ['CONCH_VERIFY_ENV'])\n"
}
```

### File Streaming

`PostFileStream` accepts a stream of `FileChunk` messages. The first chunk must include `filepath`; later chunks may omit it. The service writes to a temporary file first and commits with `rename` after the stream completes.

`GetFileStream` streams file content back in chunks. The first response chunk includes `filepath`; later chunks may leave it empty.

## Build

`conch-init` is built as a musl target so it can run inside the minimal initramfs.

```bash
make build-conch-init
```

This produces:

```text
bin/conch-init
conch-init/target/<arch>-unknown-linux-musl/release/conch-init
```

Supported host architectures:

| Host arch | Rust target |
| --- | --- |
| `x86_64` | `x86_64-unknown-linux-musl` |
| `aarch64` | `aarch64-unknown-linux-musl` |

## Build Initramfs

To package `conch-init` into an Alpine-based initramfs:

```bash
make build-conch-init-initramfs
```

Default output:

```text
build-artifacts/conch-init-initramfs.cpio.gz
```

The builder installs the binary as:

```text
/sbin/conch-init
/init -> /sbin/conch-init
```

Useful overrides:

```bash
INIT_BIN=./bin/conch-init \
OUTPUT=./build-artifacts/conch-init-initramfs.cpio.gz \
ALPINE_VERSION=3.20.3 \
./scripts/build-conch-init-initramfs.sh
```

`--agent-bin` is still accepted by the script as a deprecated alias for `--init-bin`.

## Test

Run Rust checks and unit tests:

```bash
make -C conch-init check
make -C conch-init test
```

Run gRPC verification against a live sandbox:

```bash
AGENT_GRPC_ADDR=<sandbox-ip>:4064 \
TEST_AGENT_TOKEN=<token> \
/home/conch-agent/test/verify-agent-grpc.sh
```

The verification script should cover all RPCs in `api/agent.proto`: health check, process execution, file upload, and file download.

## Operational Notes

- `conch-init` logs under `/var/log/conch-init/`.
- The rootfs service hook path is `/etc/conch/entrypoint` inside the merged rootfs.
- Rootfs services are required only when `/etc/conch/entrypoint` exists in the merged rootfs. That entrypoint marks readiness by creating `/run/conch/services-ready` and sending `SIGUSR1` to PID 1.
- The gRPC protocol retains `AgentService` naming for compatibility even though the component binary is now named `conch-init`.
- The sandbox-ready protocol still uses the `AGENT_TOKEN` field and `x-conch-agent-token` metadata for compatibility.
