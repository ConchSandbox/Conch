<img src="./docs/assets/Conch-logo.jpg" alt="Conch logo" style="width:200px;" />

<a href="https://atomgit.com/openeuler/Conch.git"><img src="https://img.shields.io/badge/atomgit-Conch-blue"/></a> ![license](https://img.shields.io/badge/license-Mulan%20PSL%20v2-blue) <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26+-blue"/> </a><a href="https://www.python.org/"><img src="https://img.shields.io/badge/Python-SDK-blue"/> </a>

# Conch - Agent Sandbox Engine


Conch is a container sandbox engine developed based on Go, designed to meet the requirements of Agents for high startup performance, high elasticity, high I/O performance, and high-density deployment of sandboxes.
The project is developed around the following new sandbox requirements of Agents:
1. New Ecosystem: Compared with traditional command-line and K8s cloud-native ecosystems, it provides Agent-native sandbox management APIs and SDKs;
2. New Image Format: Compared with the traditional OCI v1 container image format, it supports the EROFS image format to unify the management of container images and snapshots;
3. New Hardware (Super Node): Unlike traditional single-machine container image management, it leverages the high-speed interconnection capability of super nodes to provide cross-level image sharing and management mechanisms.

## Core Features

- Lightweight and Secure Isolation -- Supports virtual sandboxes to securely isolate Agent tasks. It also supports full lifecycle management, including creation, suspension, resumption, and deletion operations.
- Snapshot Boot Acceleration -- Supports snapshot functionality for virtual machine memory and root file systems. Through snapshot mechanisms, it enables second-level sandbox startup, significantly improving resource utilization efficiency in large-scale deployment scenarios. Snapshots adopt Copy-on-Write technology to minimize storage overhead.
- Streamlined Container Networking -- Uses CNI plugins for the outer sandbox network namespace while Conch keeps ownership of reusable slots, sandbox netns lifecycle, guest tap setup, and VM-side NAT. This keeps the fast network pool model while aligning the outer network boundary with CRI-style runtimes.

## Quick Start

### Prerequisites

- Go 1.26+
- Cloud-Hypervisor v51.0+
- erofs-utils 1.9+ (`mkfs.erofs --fsalignblks` is required)
- Linux 5.10+
- Root privileges or equivalent `CAP_SYS_ADMIN` and `CAP_NET_ADMIN` capabilities for network namespaces, tap devices, routes, and NAT rules
- CNI plugin binaries installed on the host, by default under `/opt/cni/bin` (`bridge`, `host-local`, and `loopback` are required for the default bridge-style setup)
- At least one Conch CNI `.conf` or `.conflist` file. The default path is `/etc/conch/cni/net.d`; if that default path is empty, Conch falls back to `cni/net.d` next to the loaded config file, such as `config/cni/net.d`.
- A CNI subnet that does not overlap with the host network, cluster network, or VM guest tap subnet
- Iptables network configuration tool. Conch still uses namespace-local NAT for the VM guest tap path; the selected CNI plugins may also require iptables or nftables depending on their configuration.

For network design details and verification steps, see [Conch Network Guide](docs/guide/net_usage.en.md).

Conchd initializes containerd services and Conch plugins in process, so a standalone system `containerd` daemon is not required.

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

Conch provides unified image management commands for converting an existing OCI rootfs image into a bootable Template, then pushing, pulling, and unpacking it:

```bash
conch template create --source docker.io/library/nginx:latest \
  --kernel ./bzImage \
  --initrd ./conch.initrd \
  -t localhost/conch/nginx:latest

conch image push localhost/conch/nginx:latest hub.oepkgs.net/conch/nginx:latest
conch image pull hub.oepkgs.net/conch/nginx:latest
# Download OCI content without creating snapshots
conch image pull --skip-unpack hub.oepkgs.net/conch/nginx:latest

# Unpack a local Conch image separately
conch image unpack hub.oepkgs.net/conch/conch-index:v0.1
```

`conch template create` converts a standard OCI rootfs image into a native EROFS rootfs, combines it with the kernel/initrd component into a Conch boot index, and registers it as a Template. `conch image pull` unpacks by default, while `--skip-unpack` downloads OCI content only; `conch image unpack` is mainly for separately unpacking local Conch images or troubleshooting.

For detailed design, see [Conch Image Workflow Design](docs/design/image-workflow.md).

### SDK Configuration

The SDK requires a configuration file to specify the connection method and sandbox parameters. A default configuration template is provided at `config/sdk-config.yaml`:

```yaml
sandbox:
  unix_socket: "/var/run/conchd/conchd.sock"   # Prefer Unix Socket connection
  api_url: "http://localhost:4063"              # Fallback HTTP connection when unix_socket is empty
  sandbox_id: ""                                # Leave empty to auto-generate
  template_id: "tmpl_xxx"                         # Template ID

image:
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

For more SDK usage, see [Python SDK API Documentation](docs/guide/python-api.md).

## License

Mulan Permissive Software License, Version 2 (Mulan PSL v2)

## Contribution Guide

Community contributions to code and documentation are warmly welcome.
