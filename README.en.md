<img src="./docs/assets/Conch-logo.jpg" alt="Conch logo" style="width:200px;" />

<a href="https://atomgit.com/openeuler/Conch.git"><img src="https://img.shields.io/badge/atomgit-Conch-blue"/></a> ![license](https://img.shields.io/badge/license-Mulan%20PSL%20v2-blue) <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.23+-blue"/> </a><a href="https://www.python.org/"><img src="https://img.shields.io/badge/Python-SDK-blue"/> </a>

# Conch - Agent Sandbox Engine

Conch is a container sandbox engine developed based on Go, designed to meet the requirements of Agents for high startup performance, high elasticity, high I/O performance, and high-density deployment of sandboxes.
The project is developed around the following new sandbox requirements of Agents:
1. New Ecosystem: Compared with traditional command-line and K8s cloud-native ecosystems, it provides Agent-native sandbox management APIs and SDKs.
2. New Image Format: Compared with the traditional OCI v1 container image format, it supports the EROFS image format to unify the management of container images and snapshots.
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

### Python SDK Example

```python
from conch import Sandbox
sandbox = Sandbox()
result = sandbox.execute(cmd="python3",content="print('hello Conch！')")
print(result)
sandbox.delete()
```

## License
Mulan Permissive Software License, Version 2 (Mulan PSL v2)

## Contribution Guide
Community contributions to code and documentation are warmly welcome.
