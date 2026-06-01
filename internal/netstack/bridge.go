/*
Copyright the Conch Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package netstack

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
	"github.com/vishvananda/netlink"
	netutils "k8s.io/utils/net"
)

const (
	defaultVrtNetworkCIDR = "10.12.0.0/20"
	defaultBridgeCount    = 1
	legacyBridgeName      = "conch_bridge"
	bridgeNamePrefix      = "conch_bridge_"
)

var (
	vrtNetworkCIDR = GetVrtNetworkCIDR()

	configuredBridgeCount int
	effectiveBridgeCount  int
	maxVrtSlotsSize       int
	maxVrtSlotIndex       int
	bridgeLayoutReady     bool
)

// Bridge layout is cached as process-global state and is configured once.
// Creating multiple pools in the same process is not supported.

func initConfigureBridgeLayout(bridgeCount int) error {
	if bridgeCount <= 0 {
		bridgeCount = defaultBridgeCount
	}
	if vrtNetworkCIDR == nil {
		return fmt.Errorf("invalid vrt network CIDR")
	}

	effective := roundUpToPowerOfTwo(bridgeCount)
	ones, _ := vrtNetworkCIDR.Mask.Size()
	prefix := ones + bits.TrailingZeros(uint(effective))
	if prefix > 30 {
		return fmt.Errorf("bridge_count=%d is too large for network %s", bridgeCount, vrtNetworkCIDR.String())
	}

	configuredBridgeCount = bridgeCount
	effectiveBridgeCount = effective
	bridgeLayoutReady = true
	slotCount, maxSlotIndex := GetVrtSlotsSizeAndIndex()
	if slotCount == invaildSlotSize || maxSlotIndex == invaildSlotSize {
		bridgeLayoutReady = false
		return fmt.Errorf("invalid bridge layout capacity")
	}

	maxVrtSlotsSize = slotCount
	maxVrtSlotIndex = maxSlotIndex
	return nil
}

func initAllBridges() (retErr error) {
	created := make([]string, 0, configuredBridgeCount)
	defer func() {
		if retErr == nil {
			return
		}
		for i := len(created) - 1; i >= 0; i-- {
			if err := deleteBridge(created[i]); err != nil {
				getLogger().Warn("failed to rollback bridge creation", ulog.F("bridge", created[i]), ulog.F("error", err))
			}
		}
	}()

	for bridgeOrdinal := 0; bridgeOrdinal < configuredBridgeCount; bridgeOrdinal++ {
		bridgeName := getBridgeName(bridgeOrdinal)
		createdBridge, err := ensureBridge(bridgeName, bridgeOrdinal)
		if err != nil {
			return err
		}
		if createdBridge {
			created = append(created, bridgeName)
		}
	}

	return nil
}

func ensureBridge(bridgeName string, bridgeOrdinal int) (bool, error) {
	bridgeNet, err := getBridgeSubnet(bridgeOrdinal)
	if err != nil {
		return false, fmt.Errorf("error getting bridge subnet for %s: %w", bridgeName, err)
	}
	bridgeIP, err := netutils.GetIndexedIP(bridgeNet, vrtAddressPerSlot)
	if err != nil {
		return false, fmt.Errorf("error getting bridge IP for %s: %w", bridgeName, err)
	}

	bridgeLink, err := netlink.LinkByName(bridgeName)
	if err == nil {
		if err := configureExistingBridge(bridgeLink, bridgeName, bridgeIP, bridgeNet.Mask); err != nil {
			return false, err
		}
		getLogger().Info("reusing existing bridge", ulog.F("bridge", bridgeName))
		return false, nil
	}

	var notFound netlink.LinkNotFoundError
	if !errors.As(err, &notFound) {
		getLogger().Error("failed to find bridge", ulog.F("bridge", bridgeName), ulog.F("error", err))
		return false, fmt.Errorf("error finding bridge %s: %w", bridgeName, err)
	}

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeName,
			MTU:  1500,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		// Another conchd/process may have created the bridge between LinkByName and LinkAdd.
		bridgeLink, lookupErr := netlink.LinkByName(bridgeName)
		if lookupErr == nil {
			if cfgErr := configureExistingBridge(bridgeLink, bridgeName, bridgeIP, bridgeNet.Mask); cfgErr != nil {
				return false, cfgErr
			}
			getLogger().Info("reusing concurrently created bridge", ulog.F("bridge", bridgeName))
			return false, nil
		}
		getLogger().Error("failed to create bridge", ulog.F("bridge", bridgeName), ulog.F("error", err))
		return false, fmt.Errorf("error creating bridge %s: %w", bridgeName, err)
	}

	bridgeLink, err = netlink.LinkByName(bridgeName)
	if err != nil {
		getLogger().Error("failed to find bridge", ulog.F("bridge", bridgeName), ulog.F("error", err))
		return true, fmt.Errorf("error finding bridge %s: %w", bridgeName, err)
	}
	if err := configureExistingBridge(bridgeLink, bridgeName, bridgeIP, bridgeNet.Mask); err != nil {
		return true, err
	}
	return true, nil
}

func configureExistingBridge(bridgeLink netlink.Link, bridgeName string, bridgeIP net.IP, bridgeMask net.IPMask) error {
	if _, ok := bridgeLink.(*netlink.Bridge); !ok {
		return fmt.Errorf("link %s already exists but is %s, not bridge", bridgeName, bridgeLink.Type())
	}
	if err := ensureBridgeAddress(bridgeLink, bridgeIP, bridgeMask); err != nil {
		return fmt.Errorf("error assigning address to bridge %s: %w", bridgeName, err)
	}
	if err := netlink.LinkSetUp(bridgeLink); err != nil {
		getLogger().Error("failed to set bridge up", ulog.F("bridge", bridgeName), ulog.F("error", err))
		return fmt.Errorf("error setting bridge %s up: %w", bridgeName, err)
	}
	return nil
}

func ensureBridgeAddress(bridgeLink netlink.Link, bridgeIP net.IP, bridgeMask net.IPMask) error {
	target := &net.IPNet{IP: bridgeIP, Mask: bridgeMask}
	hasAddress, err := bridgeHasAddress(bridgeLink, target)
	if err != nil {
		return err
	}
	if hasAddress {
		return nil
	}
	if err := netlink.AddrAdd(bridgeLink, &netlink.Addr{IPNet: target}); err != nil {
		// Treat an address that appeared concurrently as success.
		hasAddress, checkErr := bridgeHasAddress(bridgeLink, target)
		if checkErr != nil {
			return errors.Join(err, checkErr)
		}
		if hasAddress {
			return nil
		}
		return err
	}
	return nil
}

func bridgeHasAddress(bridgeLink netlink.Link, target *net.IPNet) (bool, error) {
	addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)
	if err != nil {
		return false, err
	}
	for _, addr := range addrs {
		if ipNetEqual(addr.IPNet, target) {
			return true, nil
		}
	}
	return false, nil
}

func ipNetEqual(a, b *net.IPNet) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.IP.Equal(b.IP) && string(a.Mask) == string(b.Mask)
}

func deleteBridge(bridgeName string) error {
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		getLogger().Error("failed to find bridge", ulog.F("bridge", bridgeName), ulog.F("error", err))
		return fmt.Errorf("error finding bridge %s: %w", bridgeName, err)
	}
	if _, ok := bridge.(*netlink.Bridge); !ok {
		return fmt.Errorf("link %s exists but is %s, not bridge", bridgeName, bridge.Type())
	}
	err = netlink.LinkDel(bridge)
	if err != nil {
		getLogger().Error("failed to delete bridge device", ulog.F("bridge", bridgeName), ulog.F("error", err))
		return fmt.Errorf("error delete bridge device %s: %w", bridgeName, err)
	}
	return nil
}

func deleteAllBridges() error {
	var errs []error
	for bridgeOrdinal := 0; bridgeOrdinal < configuredBridgeCount; bridgeOrdinal++ {
		bridgeName := getBridgeName(bridgeOrdinal)
		if err := deleteBridge(bridgeName); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func deleteEmptyCNIHostBridge(ctx context.Context, bridgeName string, retries int, delay time.Duration) error {
	if bridgeName == "" {
		return nil
	}
	for attempt := 0; attempt <= retries; attempt++ {
		link, err := netlink.LinkByName(bridgeName)
		if isLinkNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("finding cni bridge %s: %w", bridgeName, err)
		}
		if _, ok := link.(*netlink.Bridge); !ok {
			getLogger().Warn("skipping cni host artifact cleanup for non-bridge link", ulog.F("bridge", bridgeName), ulog.F("type", link.Type()))
			return nil
		}

		ports, err := bridgePorts(link.Attrs().Index)
		if err != nil {
			return fmt.Errorf("checking cni bridge %s ports: %w", bridgeName, err)
		}
		if len(ports) == 0 {
			if err := netlink.LinkDel(link); err != nil {
				if isLinkNotFound(err) {
					return nil
				}
				return fmt.Errorf("deleting cni bridge %s: %w", bridgeName, err)
			}
			getLogger().Info("deleted cni host bridge", ulog.F("bridge", bridgeName))
			return nil
		}
		if attempt == retries {
			return fmt.Errorf("cni bridge %s still has enslaved interfaces: %s", bridgeName, strings.Join(ports, ","))
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), fmt.Errorf("waiting for cni bridge %s to become empty", bridgeName))
		case <-time.After(delay):
		}
	}
	return nil
}

func bridgePorts(masterIndex int) ([]string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	ports := make([]string, 0)
	for _, link := range links {
		attrs := link.Attrs()
		if attrs != nil && attrs.MasterIndex == masterIndex {
			ports = append(ports, attrs.Name)
		}
	}
	return ports, nil
}

func isLinkNotFound(err error) bool {
	if err == nil {
		return false
	}
	var linkNotFound netlink.LinkNotFoundError
	return errors.As(err, &linkNotFound) || os.IsNotExist(err)
}

func getBridgeName(bridgeOrdinal int) string {
	if configuredBridgeCount == 1 {
		return legacyBridgeName
	}
	return fmt.Sprintf("%s%d", bridgeNamePrefix, bridgeOrdinal)
}

func getBridgePrefix() int {
	if !bridgeLayoutReady || effectiveBridgeCount <= 0 {
		return invaildSlotSize
	}
	ones, _ := vrtNetworkCIDR.Mask.Size()
	return ones + bits.TrailingZeros(uint(effectiveBridgeCount))
}

func getSlotsPerBridge() int {
	prefix := getBridgePrefix()
	if prefix <= 0 || prefix > 30 {
		return invaildSlotSize
	}
	hostsPerBridge := 1 << (32 - prefix)
	// Reserve subnet base (.0), bridge IP (.1), and subnet broadcast.
	return hostsPerBridge - 3
}

func getBridgeSubnet(bridgeOrdinal int) (*net.IPNet, error) {
	if vrtNetworkCIDR == nil {
		return nil, fmt.Errorf("invalid vrt network CIDR")
	}
	if bridgeOrdinal < 0 || bridgeOrdinal >= configuredBridgeCount {
		return nil, fmt.Errorf("bridge ordinal %d is out of range", bridgeOrdinal)
	}

	baseIP := vrtNetworkCIDR.IP.To4()
	if baseIP == nil {
		return nil, fmt.Errorf("vrt network CIDR %q is not IPv4", vrtNetworkCIDR.String())
	}

	prefix := getBridgePrefix()
	subnetSize := uint32(1 << (32 - prefix))
	base := binary.BigEndian.Uint32(baseIP)
	ip := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(ip, base+uint32(bridgeOrdinal)*subnetSize)
	return &net.IPNet{
		IP:   ip,
		Mask: net.CIDRMask(prefix, 32),
	}, nil
}

func roundUpToPowerOfTwo(v int) int {
	if v <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(v-1))
}

func GetVrtNetworkCIDR() *net.IPNet {
	_, vrtIP, err := net.ParseCIDR(defaultVrtNetworkCIDR)
	if err != nil {
		getLogger().Error("failed to parse vrt network CIDR", ulog.F("cidr", defaultVrtNetworkCIDR), ulog.F("error", err))
		return nil
	}
	return vrtIP
}

func GetVrtSlotsSizeAndIndex() (slotCount int, maxSlotIndex int) {
	if !bridgeLayoutReady {
		return invaildSlotSize, invaildSlotSize
	}

	slotsPerBridge := getSlotsPerBridge()
	if configuredBridgeCount <= 0 || slotsPerBridge == invaildSlotSize {
		return invaildSlotSize, invaildSlotSize
	}

	slotCount = configuredBridgeCount * slotsPerBridge
	maxSlotIndex = firstSlotIndex + slotCount - 1
	getLogger().Info(
		"Using network slot size",
		ulog.F("total_slots", slotCount),
		ulog.F("max_slot_index", maxSlotIndex),
		ulog.F("bridge_count", configuredBridgeCount),
		ulog.F("effective_bridge_count", effectiveBridgeCount),
	)
	return slotCount, maxSlotIndex
}
