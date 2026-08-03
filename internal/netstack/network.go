/*
Copyright the e2b-dev Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

[MODIFIED] - Changes made on 2025-12-24 by Team conch: Add veth into bridge and optimize iptables config
[MODIFIED] - Changes made on 2026-05-13 by Team conch: Split CNI-owned outer networking from Conch-owned guest tap networking
*/
package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const ipv4ForwardingSysctlPath = "/proc/sys/net/ipv4/ip_forward"

func prepareNetworkNamespaceDirectory() error {
	if err := os.MkdirAll(netNamespacesDir, 0o700); err != nil {
		return fmt.Errorf("create Conch network namespace directory: %w", err)
	}
	if err := os.Chmod(netNamespacesDir, 0o700); err != nil {
		return fmt.Errorf("secure Conch network namespace directory: %w", err)
	}
	return nil
}

func createNetworkNamespace(slotID int) (retErr error) {
	if err := validateSlotID(slotID); err != nil {
		return err
	}
	netnsPath := networkNamespacePath(slotID)
	target, err := os.OpenFile(netnsPath, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
	if err != nil {
		return fmt.Errorf("reserve network namespace path %s: %w", netnsPath, err)
	}
	if err := target.Close(); err != nil {
		_ = os.Remove(netnsPath)
		return fmt.Errorf("close network namespace path %s: %w", netnsPath, err)
	}

	runtime.LockOSThread()
	hostNS, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		_ = os.Remove(netnsPath)
		return fmt.Errorf("cannot get current (host) namespace: %w", err)
	}
	newNS := netns.None()
	mounted := false
	defer func() {
		if err := netns.Set(hostNS); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error resetting network namespace back to the host namespace: %w", err))
		}
		if newNS.IsOpen() {
			if err := newNS.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("error closing new network namespace: %w", err))
			}
		}
		if err := hostNS.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error closing host network namespace: %w", err))
		}
		runtime.UnlockOSThread()
		if retErr != nil {
			if mounted {
				_ = unix.Unmount(netnsPath, unix.MNT_DETACH)
			}
			_ = os.Remove(netnsPath)
		}
	}()

	newNS, err = netns.New()
	if err != nil {
		return fmt.Errorf("cannot create new namespace: %w", err)
	}
	nsPath := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
	if err := unix.Mount(nsPath, netnsPath, "bind", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mount network namespace at %s: %w", netnsPath, err)
	}
	mounted = true

	return nil
}

// cniNetworkNamespacePath returns an empty path when the Slot namespace is
// absent or was only reserved but never bind-mounted. CNI DEL accepts an empty
// namespace path for best-effort cleanup such as releasing host-local IPAM.
func cniNetworkNamespacePath(netnsPath string) string {
	var stat unix.Statfs_t
	if err := unix.Statfs(netnsPath, &stat); err != nil {
		return ""
	}
	if stat.Type != unix.NSFS_MAGIC && stat.Type != unix.PROC_SUPER_MAGIC {
		return ""
	}
	return netnsPath
}

func DeleteSandboxNetworkNamespace(slotID int) error {
	if err := validateSlotID(slotID); err != nil {
		return err
	}
	netnsPath := networkNamespacePath(slotID)
	if _, err := os.Stat(netnsPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("error checking namespace %s: %w", netnsPath, err)
	}
	if err := unix.Unmount(netnsPath, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("error deleting namespace %s: %w", netnsPath, err)
	}
	if err := os.Remove(netnsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove namespace path %s: %w", netnsPath, err)
	}
	return nil
}

func runInNetNSPath(netnsPath string, fn func() error) (retErr error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("cannot get current namespace: %w", err)
	}
	defer func() {
		if err := netns.Set(hostNS); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error resetting network namespace back to host: %w", err))
		}
		if err := hostNS.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error closing host network namespace: %w", err))
		}
	}()

	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("cannot open network namespace %s: %w", netnsPath, err)
	}
	defer func() {
		if err := targetNS.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error closing target network namespace %s: %w", netnsPath, err))
		}
	}()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("error setting network namespace to %s: %w", netnsPath, err)
	}
	return fn()
}

func ValidateReusableSlotNetwork(ctx context.Context, slot *Slot, netnsPath string, ifName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	if ifName == "" {
		ifName = defaultCNIIfName
	}
	return runInNetNSPath(netnsPath, func() error {
		cniLink, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("cni interface %s missing: %w", ifName, err)
		}
		if result := slot.CNIResult(); result != nil && result.IP != "" {
			expectedIP := parseCNIResultIP(result.IP)
			if expectedIP == nil {
				return fmt.Errorf("stored cni IP %q is invalid", result.IP)
			}
			hasIP, err := linkHasIP(cniLink, expectedIP)
			if err != nil {
				return fmt.Errorf("checking cni interface %s addresses: %w", ifName, err)
			}
			if !hasIP {
				return fmt.Errorf("cni interface %s missing stored IP %s", ifName, expectedIP.String())
			}
		}
		if _, err := netlink.LinkByName(slot.TapName()); err != nil {
			return fmt.Errorf("tap interface %s missing: %w", slot.TapName(), err)
		}
		return nil
	})
}

func parseCNIResultIP(raw string) net.IP {
	if ip := net.ParseIP(raw); ip != nil {
		return ip
	}
	ip, _, err := net.ParseCIDR(raw)
	if err != nil {
		return nil
	}
	return ip
}

func linkHasIP(link netlink.Link, ip net.IP) (bool, error) {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return false, err
	}
	for _, addr := range addrs {
		if addr.IP.Equal(ip) {
			return true, nil
		}
	}
	return false, nil
}

func SetupGuestTapNetwork(ctx context.Context, slot *Slot, netnsPath string, cniResult *CNIResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	return runInNetNSPath(netnsPath, func() error {
		return slot.setupGuestTapNetwork(cniResult)
	})
}

func TeardownGuestTapNetwork(ctx context.Context, slot *Slot, netnsPath string, cniResult *CNIResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	return runInNetNSPath(netnsPath, func() error {
		return slot.teardownGuestTapNetwork(cniResult)
	})
}

func (s *Slot) setupGuestTapNetwork(cniResult *CNIResult) error {
	if cniResult == nil || cniResult.IP == "" {
		return fmt.Errorf("cni result has no sandbox outer IP")
	}

	tapAttrs := netlink.NewLinkAttrs()
	tapAttrs.Name = s.TapName()
	tap := &netlink.Tuntap{
		Mode:      netlink.TUNTAP_MODE_TAP,
		LinkAttrs: tapAttrs,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("error creating tap device: %w", err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("error setting tap device up: %w", err)
	}
	if err := netlink.AddrAdd(tap, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   s.TapIP(),
			Mask: s.TapCIDR(),
		},
	}); err != nil {
		return fmt.Errorf("error setting address of the tap device: %w", err)
	}

	lo, err := netlink.LinkByName(loopbackInterface)
	if err != nil {
		return fmt.Errorf("error finding lo: %w", err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return fmt.Errorf("error setting lo device up: %w", err)
	}

	if err := os.WriteFile(ipv4ForwardingSysctlPath, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("error enabling ipv4 forwarding via %s: %w", ipv4ForwardingSysctlPath, err)
	}

	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}
	if err := tables.Append("nat", "POSTROUTING", "-s", s.NamespaceIP(), "-j", "SNAT", "--to", cniResult.IP); err != nil {
		return fmt.Errorf("error creating postrouting rule to guest tap: %w", err)
	}
	if err := tables.Append("nat", "PREROUTING", "-d", cniResult.IP, "-j", "DNAT", "--to-destination", s.NamespaceIP()); err != nil {
		return fmt.Errorf("error creating prerouting rule to guest tap: %w", err)
	}

	return nil
}

func (s *Slot) teardownGuestTapNetwork(cniResult *CNIResult) error {
	var errs []error

	if cniResult != nil && cniResult.IP != "" {
		tables, err := iptables.New()
		if err != nil {
			errs = append(errs, fmt.Errorf("error initializing iptables: %w", err))
		} else {
			if err := tables.DeleteIfExists("nat", "POSTROUTING", "-s", s.NamespaceIP(), "-j", "SNAT", "--to", cniResult.IP); err != nil {
				errs = append(errs, fmt.Errorf("error deleting postrouting rule to guest tap: %w", err))
			}
			if err := tables.DeleteIfExists("nat", "PREROUTING", "-d", cniResult.IP, "-j", "DNAT", "--to-destination", s.NamespaceIP()); err != nil {
				errs = append(errs, fmt.Errorf("error deleting prerouting rule to guest tap: %w", err))
			}
		}
	}

	tap, err := netlink.LinkByName(s.TapName())
	if err == nil {
		if err := netlink.LinkDel(tap); err != nil {
			errs = append(errs, fmt.Errorf("error deleting tap device: %w", err))
		}
	} else {
		var linkNotFound netlink.LinkNotFoundError
		if !errors.As(err, &linkNotFound) && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("error finding tap device: %w", err))
		}
	}

	return errors.Join(errs...)
}
