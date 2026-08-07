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

[MODIFIED] - Changes made on 2026-08-05 by Team conch: Encapsulate Conch-owned guest tap networking.
*/
package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
)

const (
	ipv4ForwardingSysctlPath = "/proc/sys/net/ipv4/ip_forward"
	loopbackInterface        = "lo"
)

func checkGuestTapNetwork(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	return runInNetNSPath(ctx, slot.NetNSPath(), func() error {
		if _, err := netlink.LinkByName(slot.TapName()); err != nil {
			return fmt.Errorf("tap interface %s missing: %w", slot.TapName(), err)
		}
		return nil
	})
}

func configureGuestTapNetwork(slot *Slot, cniIP string) error {
	if cniIP == "" {
		return fmt.Errorf("cni IP is required")
	}

	tapAttrs := netlink.NewLinkAttrs()
	tapAttrs.Name = slot.TapName()
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
			IP:   slot.tapAddress(),
			Mask: slot.tapMaskValue(),
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
	if err := tables.Append("nat", "POSTROUTING", "-s", slot.namespaceAddress(), "-j", "SNAT", "--to", cniIP); err != nil {
		return fmt.Errorf("error creating postrouting rule to guest tap: %w", err)
	}
	if err := tables.Append("nat", "PREROUTING", "-d", cniIP, "-j", "DNAT", "--to-destination", slot.namespaceAddress()); err != nil {
		return fmt.Errorf("error creating prerouting rule to guest tap: %w", err)
	}

	return nil
}

func removeGuestTapNetwork(slot *Slot, cniIP string) error {
	var errs []error

	if cniIP != "" {
		tables, err := iptables.New()
		if err != nil {
			errs = append(errs, fmt.Errorf("error initializing iptables: %w", err))
		} else {
			if err := tables.DeleteIfExists("nat", "POSTROUTING", "-s", slot.namespaceAddress(), "-j", "SNAT", "--to", cniIP); err != nil {
				errs = append(errs, fmt.Errorf("error deleting postrouting rule to guest tap: %w", err))
			}
			if err := tables.DeleteIfExists("nat", "PREROUTING", "-d", cniIP, "-j", "DNAT", "--to-destination", slot.namespaceAddress()); err != nil {
				errs = append(errs, fmt.Errorf("error deleting prerouting rule to guest tap: %w", err))
			}
		}
	}

	tap, err := netlink.LinkByName(slot.TapName())
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
