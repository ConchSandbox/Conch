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
*/
package netstack

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const ipv4ForwardingSysctlPath = "/proc/sys/net/ipv4/ip_forward"

func enableIPv4Forwarding(sysctlPath string) error {
	if err := os.WriteFile(sysctlPath, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("error enabling ipv4 forwarding via %s: %w", sysctlPath, err)
	}
	return nil
}

func (s *Slot) addVethToBridge(veth *netlink.Veth, hostNS netns.NsHandle) error {
	// Move Veth device to the host NS
	err := netlink.LinkSetNsFd(veth, int(hostNS))
	if err != nil {
		return fmt.Errorf("error moving veth device to the host namespace: %w", err)
	}
	err = netns.Set(hostNS)
	if err != nil {
		return fmt.Errorf("error setting network namespace: %w", err)
	}

	// add veth into bridge
	vethInHost, err := netlink.LinkByName(s.VethName())
	if err != nil {
		return fmt.Errorf("error finding veth: %w", err)
	}
	err = netlink.LinkSetUp(vethInHost)
	if err != nil {
		return fmt.Errorf("error setting veth device up: %w", err)
	}
	bridgeLink, err := netlink.LinkByName(s.BridgeName())
	if err != nil {
		return fmt.Errorf("error finding bridge %s: %w", s.BridgeName(), err)
	}
	if err := netlink.LinkSetMaster(vethInHost, bridgeLink); err != nil {
		return fmt.Errorf("error add veth %v into bridge %v: %w", vethInHost, bridgeLink, err)
	}

	return nil
}

func (s *Slot) initSandboxNetwork(tables *iptables.IPTables, sandboxNs netns.NsHandle) error {
	err := netns.Set(sandboxNs)
	if err != nil {
		return fmt.Errorf("error setting network namespace to %s: %w", sandboxNs.String(), err)
	}
	// create tap interface
	tapAttrs := netlink.NewLinkAttrs()
	tapAttrs.Name = s.TapName()
	tapAttrs.Namespace = sandboxNs
	tap := &netlink.Tuntap{
		Mode:      netlink.TUNTAP_MODE_TAP,
		LinkAttrs: tapAttrs,
	}
	err = netlink.LinkAdd(tap)
	if err != nil {
		return fmt.Errorf("error creating tap device: %w", err)
	}
	err = netlink.LinkSetUp(tap)
	if err != nil {
		return fmt.Errorf("error setting tap device up: %w", err)
	}
	err = netlink.AddrAdd(tap, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   s.TapIP(),
			Mask: s.TapCIDR(),
		},
	})
	if err != nil {
		return fmt.Errorf("error setting address of the tap device: %w", err)
	}

	// set NS lo device up
	lo, err := netlink.LinkByName(loopbackInterface)
	if err != nil {
		return fmt.Errorf("error finding lo: %w", err)
	}
	err = netlink.LinkSetUp(lo)
	if err != nil {
		return fmt.Errorf("error setting lo device up: %w", err)
	}

	// DNAT to the guest tap address requires IPv4 forwarding inside the sandbox netns.
	if err := enableIPv4Forwarding(ipv4ForwardingSysctlPath); err != nil {
		return err
	}

	// Add NS default route
	link, err := netlink.LinkByName(s.VpeerName())
	if err != nil {
		return fmt.Errorf("error finding vpeer: %w", err)
	}
	linkIndex := link.Attrs().Index
	err = netlink.RouteAdd(&netlink.Route{
		LinkIndex: linkIndex,
		Gw:        s.BridgeIP(),
	})
	if err != nil {
		return fmt.Errorf("error adding default NS route: %w", err)
	}

	// Add NAT routing rules to NS
	err = tables.Append("nat", "POSTROUTING", "-s", s.NamespaceIP(), "-j", "SNAT", "--to", s.VpeerIPString())
	if err != nil {
		return fmt.Errorf("error creating postrouting rule to vpeer: %w", err)
	}
	err = tables.Append("nat", "PREROUTING", "-d", s.VpeerIPString(), "-j", "DNAT", "--to-destination", s.NamespaceIP())
	if err != nil {
		return fmt.Errorf("error creating postrouting rule from vpeer: %w", err)
	}

	return nil
}

func (s *Slot) setHostRoute(hostNS netns.NsHandle) error {
	err := netns.Set(hostNS)
	if err != nil {
		return fmt.Errorf("error setting network namespace to %s: %w", hostNS.String(), err)
	}
	return nil
}

func (s *Slot) CreateNetwork() (retErr error) {
	// prevent thread changes so we can safely manipulate with namespaces
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// get host namespace
	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("cannot get current (host) namespace: %w", err)
	}
	defer func() {
		err = netns.Set(hostNS)
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error resetting network namespace back to the host namespace: %w", err))
		}

		err = hostNS.Close()
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("error closing host network namespace: %w", err))
		}
	}()

	// Create NS for the sandbox and enter new network NS
	ns, err := netns.NewNamed(s.NamespaceID())
	if err != nil {
		return fmt.Errorf("cannot create new namespace: %w", err)
	}
	defer func() {
		ns.Close()
		if retErr != nil {
			nsErr := netns.DeleteNamed(s.NamespaceID())
			if nsErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("error deleting namespace: %w", nsErr))
			}
		}
	}()

	// Create the Veth and Vpeer
	vethAttrs := netlink.NewLinkAttrs()
	vethAttrs.Name = s.VethName()
	veth := &netlink.Veth{
		LinkAttrs: vethAttrs,
		PeerName:  s.VpeerName(),
	}
	err = netlink.LinkAdd(veth)
	if err != nil {
		return fmt.Errorf("error creating veth device: %w", err)
	}
	defer func() {
		if retErr != nil {
			err = netns.Set(hostNS)
			if err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("error setting host network namespace: %w", err))
				return
			}
			if link, err := netlink.LinkByName(s.VethName()); err == nil {
				if err := netlink.LinkDel(link); err != nil && !os.IsNotExist(err) {
					retErr = errors.Join(retErr, fmt.Errorf("delete veth failed: %w", err))
				}
			}
		}
	}()
	vpeer, err := netlink.LinkByName(s.VpeerName())
	if err != nil {
		return fmt.Errorf("error finding vpeer: %w", err)
	}
	err = netlink.LinkSetUp(vpeer)
	if err != nil {
		return fmt.Errorf("error setting vpeer device up: %w", err)
	}
	// add IP for ns-veth-%{idx}
	err = netlink.AddrAdd(vpeer, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   s.VpeerIP(),
			Mask: s.VrtMask(),
		},
	})
	if err != nil {
		return fmt.Errorf("error adding vpeer device address: %w", err)
	}

	if err = s.addVethToBridge(veth, hostNS); err != nil {
		return fmt.Errorf("error adding veth to bridge: %w", err)
	}
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}
	if err := s.initSandboxNetwork(tables, ns); err != nil {
		return fmt.Errorf("error init sandbox network: %w", err)
	}
	// todo: init nftables
	// if the netns network where the sandbox is located needs to perform traffic filtering and forwarding later,
	// the firewall needs to be enabled.

	return s.setHostRoute(hostNS)
}

func (s *Slot) RemoveNetwork() error {
	var errs []error

	// delete veth device
	veth, err := netlink.LinkByName(s.VethName())
	if err != nil {
		var linkNotFound netlink.LinkNotFoundError
		if !errors.As(err, &linkNotFound) {
			errs = append(errs, fmt.Errorf("error finding veth: %w", err))
		}
	} else {
		err = netlink.LinkDel(veth)
		if err != nil {
			errs = append(errs, fmt.Errorf("error deleting veth device: %w", err))
		}
	}
	// delete ns
	err = netns.DeleteNamed(s.NamespaceID())
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("error deleting namespace: %w", err))
	}

	return errors.Join(errs...)
}
