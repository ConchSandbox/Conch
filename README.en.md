<img src="./docs/assets/Conch-logo.jpg" alt="Conch logo" style="width:200px;" />

<a href="https://atomgit.com/openeuler/Conch.git"><img src="https://img.shields.io/badge/atomgit-Conch-blue"/></a> ![license](https://img.shields.io/badge/license-Mulan%20PSL%20v2-blue) <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.23+-blue"/> </a><a href="https://www.python.org/"><img src="https://img.shields.io/badge/Python-SDK-blue"/> </a>

# Conch - Agent Sandbox Engine


Conch is a container sandbox engine developed based on Go, designed to meet the requirements of Agents for high startup performance, high elasticity, high I/O performance, and high-density deployment of sandboxes.
The project is developed around the following new sandbox requirements of Agents:
1. New Ecosystem: Compared with traditional command-line and K8s cloud-native ecosystems, it provides Agent-native sandbox management APIs and SDKs;
2. New Image Format: Compared with the traditional OCI v1 container image format, it supports the EROFS image format to unify the management of container images and snapshots;
3. New Hardware (Super Node): Unlike traditional single-machine container image management, it leverages the high-speed interconnection capability of super nodes to provide cross-level image sharing and management mechanisms.

## Core Features

- Lightweight and Secure Isolation -- Supports virtual sandboxes to securely isolate Agent tasks. It also supports full lifecycle management, including creation, suspension, resumption, and deletion operations.
- Snapshot Boot Acceleration -- Supports snapshot functionality for virtual machine memory and root file systems. Through snapshot mechanisms, it enables second-level sandbox startup, significantly improving resource utilization efficiency in large-scale deployment scenarios. Snapshots adopt Copy-on-Write technology to minimize storage overhead.
- Streamlined Container Networking -- Implements network isolation and address translation through veth devices and NAT rules. It supports pooled reuse of container networks to reduce startup latency.

## Quick Start

### Prerequisites

- Go 1.23+
- Containerd 2.2.1+
- Cloud-Hypervisor v48.0+
- Iptables network configuration tool
- Linux 5.10+

### One-Click Compilation and Installation


```bash
# Clone the code repository
git clone https://atomgit.com/openeuler/Conch.git
cd Conch
git checkout demo

# Execute the full process with one click
./scripts/conch-env-setup.sh all

pip install -e ./sdk
```

### Run the Service

After compilation, the binary files are located in the `bin/` directory. Start the conchd service with the following command:

```bash
./bin/conchd
```

### Image Management

Conch provides unified image management commands for building, pushing, pulling, and unpacking Conch images:

```bash
conch build -f Dockerfile -t localhost/demo-sandbox:latest .
conch push localhost/demo-sandbox:latest hub.oepkgs.net/conch/demo-sandbox:latest
conch pull hub.oepkgs.net/conch/demo-sandbox:latest
conch pull docker.io/library/nginx:latest

# Unpack a local Conch image separately
conch unpack hub.oepkgs.net/conch/conch-index:v0.1
```

`conch pull` automatically unpacks after pulling; `conch unpack` is mainly for separately unpacking local Conch images or troubleshooting.

For detailed usage, see [Conch Image Guide](docs/guide/image.md).

### SDK Configuration

The SDK requires a configuration file to specify the connection method and sandbox parameters. A default configuration template is provided at `config/sdk-config.yaml`:

```yaml
sandbox:
  unix_socket: "/var/run/conchd/conchd.sock"   # Prefer Unix Socket connection
  api_url: "http://localhost:4063"              # Fallback HTTP connection when unix_socket is empty
  sandbox_id: ""                                # Leave empty to auto-generate

snapshot:
  snapshot_id: ""                               # Snapshot ID (optional: for snapshot boot)
  vmm_name: "cloud-hypervisor"                  # Virtual machine monitor name
  vcpu_num: 1                                   # Number of virtual CPUs
  ram_mb: 1024                                  # Memory size in MB

image:
  image_name: "hub.oepkgs.net/conch/openeuler:odd-x86"  # Image name, replace as needed
  # image_name: "hub.oepkgs.net/conch/openeuler:odd-aarch"  # aarch64 architecture image
  use_snapshot: false                           # Set to true for snapshot image boot
  vmm_name: "cloud-hypervisor"                  # Virtual machine monitor name
  vcpu_num: 1                                   # Number of virtual CPUs
  ram_mb: 1024                                  # Memory size in MB
```

After installing the SDK, you can initialize system-level configuration (optional):

```bash
sudo conch-sdk-init-config        # Copy template to /etc/conch/sdk-config.yaml
sudo conch-sdk-init-config -f     # Force overwrite existing configuration
```

Configuration file loading priority (from highest to lowest):

| Priority | Method | Description |
|----------|--------|-------------|
| 1 | `Sandbox.create(config_path="...")` | Specify directly in code, skip auto-search |
| 2 | `$CONCH_SDK_CONFIG` env variable | Specify path via environment variable |
| 3 | `~/.config/conch/sdk-config.yaml` | User-level configuration |
| 4 | `/etc/conch/sdk-config.yaml` | System-level configuration |
| 5 | `<repo>/config/sdk-config.yaml` | Built-in repository template |

### Python SDK Example

```python
from conch import Sandbox

try:
    sandbox = Sandbox.create()
    print(f"Sandbox created: {sandbox.sandbox_id}")

    # Get sandbox info
    sandbox.get_info()

    # Execute a Python script
    result = sandbox.execute(cmd="python3", content="print('hello Conch!')")
    print(result)

    # Execute a system command with arguments
    result = sandbox.execute(cmd="ls", args=["-l", "/root"])
    print(result)
except RuntimeError as e:
    print(f"Error: {e}")
finally:
    sandbox.delete()
```

You must call `Sandbox.create()` successfully before `execute()`, and make sure `./bin/conchd` is already running; otherwise the `Sandbox` instance has not been bound to an available Agent client.

For more SDK usage, see [Python SDK API Documentation](docs/python-api.md).

## License

Mulan Permissive Software License, Version 2 (Mulan PSL v2)

## Contribution Guide

Community contributions to code and documentation are warmly welcome.
