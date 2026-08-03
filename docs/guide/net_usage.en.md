# Conch Network Usage

This guide describes how to configure and verify the current Conch network design. It mainly targets a single-machine Linux development environment and does not require Kubernetes.

## Basic Design

- Conch creates reusable network slots and network namespaces.
- Conch calls CNI `ADD` for each slot with a dedicated CNI ID, such as `conch-slot-2`, instead of using a temporary sandbox ID.
- CNI creates the outer sandbox interface, normally `eth0`, including bridge/veth, IPAM, routes, and policy.
- Conch then creates the VM-facing `tap`, enables forwarding inside the namespace, and installs local forwarding rules between the outer IP and the IP inside the tap subnet.
- When a slot is explicitly removed, Conch removes tap/NAT first, calls CNI `DEL`, deletes the network namespace, and finally removes the empty Conch-owned CNI bridge link selected from the CNI config.

## Host Setup

- Run `conchd` as root, or provide equivalent `CAP_SYS_ADMIN` and `CAP_NET_ADMIN` capabilities.
- Install the CNI plugin binaries under `/usr/libexec/cni`, or under the path configured by `network.cni.plugin_bin_dirs`: `bridge`, `host-local`, and `loopback`.
- Put Conch CNI configs in a Conch-only config directory. The default directory is `/etc/conch/cni/net.d`.

The repository provides an example config at `config/cni/net.d/10-conch.conf`. If `network.cni.plugin_conf_dir` keeps the default value and `/etc/conch/cni/net.d` has no CNI config files, Conch falls back to `cni/net.d` next to the loaded Conch config file. When using the repository `config/config.yaml`, that fallback path is `config/cni/net.d`.

## CNI Config Example

The repository example is a bridge-style CNI config:

```json
{
  "cniVersion": "1.0.0",
  "name": "conch-bridge",
  "type": "bridge",
  "bridge": "cni-conch0",
  "isGateway": true,
  "ipMasq": true,
  "ipam": {
    "type": "host-local",
    "subnet": "10.12.0.0/20",
    "routes": [{ "dst": "0.0.0.0/0" }]
  }
}
```

The `bridge` field must be explicit, so cleanup can find and remove the matching CNI bridge link. Do not point Conch at a directory that also contains CNI configs for other runtimes.

## Conch Network Config Example

The network-related section in `config/config.yaml` is:

```yaml
network:
  warm_pool_size: 250
  dynamic_reservation: false
  tap_ip: 192.168.100.2
  tap_mask: 24
  cni:
    plugin_bin_dirs:
      - /usr/libexec/cni
    plugin_conf_dir: /etc/conch/cni/net.d
    plugin_max_conf: 1
    if_name: eth0
    setup_serially: false
```

Pay attention to the following network config items:

- `warm_pool_size` sets the number of ready-to-use idle network slots to pre-create and maintain and cannot exceed the code-defined total capacity of 4000 slots.
- At startup, Conch rebuilds its in-memory ID allocator from all BoltDB Slot records—including transient, idle, and assigned slots. Those records share the fixed total capacity of 4000; CNI/IPAM address capacity may impose a lower effective limit.
- With `dynamic_reservation` enabled, a CNI allocation failure rolls back the attempted slot and is retried with exponential backoff. Sandbox creation reports an unavailable resource when the pool is empty.
- `tap_ip` and `tap_mask` define the VM-facing `tap` subnet inside each sandbox.
- `plugin_bin_dirs` points to the CNI plugin directory, and `plugin_conf_dir` points to the Conch CNI config directory.
- `if_name` sets the sandbox network interface name created by CNI, normally `eth0`; current go-cni creates the first interface as `<prefix>0`, so this value must match that naming pattern.

## Manual Verification

Start `conchd` as root and verify network pool prefill:

```bash
sudo ./bin/conchd -config config/config.yaml
sudo ls -1 /run/conch/netns
```

- Expected result: namespace handles for prefilled slots are visible, such as `slot-2`. Conch owns this directory exclusively, so these handles do not appear in `ip netns list`, which scans `/run/netns`.

Inspect one namespace:

```bash
sudo nsenter --net=/run/conch/netns/slot-2 -- ip addr show eth0
sudo nsenter --net=/run/conch/netns/slot-2 -- ip route
sudo nsenter --net=/run/conch/netns/slot-2 -- ip addr show tap0
sudo nsenter --net=/run/conch/netns/slot-2 -- sysctl net.ipv4.ip_forward
sudo nsenter --net=/run/conch/netns/slot-2 -- iptables -t nat -S
```

- Expected result:
  - `eth0` is **up** and has an IP from the CNI subnet, such as `10.12.0.2/20`.
  - The default route points to the CNI bridge gateway, such as `10.12.0.1`.
  - `tap0` exists and has the configured tap IP, such as `192.168.100.2/24`.
  - `net.ipv4.ip_forward = 1`.
  - Namespace-local NAT rules map the outer CNI IP to the guest IP inside the tap subnet, such as `192.168.100.21`.

Inspect the host bridge and CNI IPAM state:

```bash
ip link show cni-conch0
ls /var/lib/cni/networks/conch-bridge
```

- Expected result: while slots exist, the bridge is up, and the host-local IPAM directory has one allocation file for each CNI-assigned IP. `last_reserved_ip.*` and `lock` are host-local metadata, not leaked slot allocations.

After normal `conchd` shutdown:

```bash
sudo ls -1 /run/conch/netns
ip link show cni-conch0
ip route show table main | grep 10.12
ip neigh show | grep 10.12
ls /var/lib/cni/networks/conch-bridge
```

- Expected result: normal exit, including `SIGTERM` and `SIGINT`, preserves network resources so they can be adopted again after restart. Netns entries, CNI IPAM allocation files, and the CNI bridge may continue to exist. After restarting `conchd`, preserved warm slots should be adopted, and slots assigned to sandboxes should be restored according to the sandbox state.

## Manual Cleanup

Normal `conchd` exit preserves all network resources. If a full reset is needed, meaning no sandbox, snapshot, network, or other state should be kept, stop `conchd`, delete data record `/var/lib/conch/state.db`, and manually remove the namespace, bridge, and IPAM directory:

```bash
sudo sh -c '
for ns in /run/conch/netns/slot-*; do
  [ -e "$ns" ] || continue
  umount -l "$ns"
  rm -f "$ns"
done
rmdir /run/conch/netns 2>/dev/null || true
'
sudo ip link delete cni-conch0 2>/dev/null || true
sudo rm -rf /var/lib/cni/networks/conch-bridge
```

If `iptables -t nat -S | grep CNI` still shows CNI chains, delete the jump rules that reference those chains first, then flush and delete the empty chains. Removing only the IPAM directory does not clean iptables rules.
