# Conch Netstack

Conch uses a daemon-owned network pool with prefilled slots ready to be leased to sandboxes.

- A "slot" is the unit of the Conch network pool. Each slot represents one reusable sandbox network namespace and its prepared network state:
  - a stable slot identity, such as `conch-slot-2`
  - a Conch-owned network namespace handle, such as `/run/conch/netns/slot-2`
  - the CNI-created outer sandbox interface, normally `eth0`
  - the CNI-assigned outer IP, routes, and IPAM allocation
  - the VM-facing `tap0` interface
  - the guest tap subnet/IP plan
  - namespace-local forwarding and NAT between the CNI IP and guest tap IP

The numeric Slot ID is the slot's sole identity. Pool allocates IDs with an in-memory min-heap and occupancy bitmap. Conch creates a fresh in-memory allocator and warm pool on every startup; it does not restore or adopt old Slots from BoltDB. The CNI ID and netns path are derived from the Slot ID instead of being stored as independently mutable values.

Network ownership is split as follows:
- Conch owns reusable slot allocation, sandbox network namespace lifecycle, VM guest `tap0`, namespace-local forwarding, and NAT between the CNI-provided outer IP and the guest IP on the tap subnet.
- CNI owns creation and removal of outer sandbox interface, normally `eth0`: bridge attachment, veth creation, IPAM, routes, DNS returned by the plugin, and plugin-managed host egress policy.

Internal responsibilities are divided as follows:
- `pool.go` orchestrates Slot creation, health checks, destruction, initial prefill, and continuous refill.
- `cni.go` encapsulates CNI ADD and DEL together with outer-interface validation. Conch loads exactly one non-loopback CNI configuration; with the built-in loopback network, the fixed minimum network count is 2.
- `netns.go` creates, enters, and deletes network namespaces.
- `guest_tap.go` manages the VM-facing `tap0`, IPv4 forwarding, and NAT.
- `slot.go` stores Slot state and derived addressing. State mutations are private to the `netstack` package; Sandbox only reads the Slot ID, CNI IP, netns path, and tap name through read-only methods.
- The `slot` subpackage only provides the ID allocator and warm queue. It contains no network domain state and performs no CNI, netlink, or iptables operations.

## Lineage and Change

The reusable slot pool, network namespace lifecycle, and VM guest tap/NAT model are inspired by the [E2B sandbox network design](https://github.com/e2b-dev/infra/tree/main/packages/orchestrator/internal/sandbox/network). Conch keeps those ideas because they are useful for fast sandbox startup and slot reuse.

The major change in this package is the Sandbox/CNI responsibility split: Conch no longer directly builds the outer bridge/veth/IP/route stack. That outer sandbox networking boundary is now delegated to CNI plugins, while Conch remains responsible for the VM edge.

See [docs/guide/net_usage.en.md](../../docs/guide/net_usage.en.md) for host requirements, CNI configuration, runtime flow, and manual verification steps.
