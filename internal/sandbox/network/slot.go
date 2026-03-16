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

[MODIFIED] - Changes made on 2025-12-24 by Team conch: Simplify slot config and init bridge
*/
package network

import (
	"fmt"
	"net"
	"sync"

	"github.com/vishvananda/netlink"

	"github.com/openeuler/Conch/pkg/ulog"
	netutils "k8s.io/utils/net"
)

const (
	defaultVrtNetworkCIDR = "10.12.0.0/20"
	vrtMask               = 20
	invaildSlotSize       = 0
	vrtAddressPerSlot     = 1
	firstSlotIndex        = 2
	tapInterfaceName      = "tap0"
	tapIp                 = "192.168.100.2"
	tapMask               = 20
	loopbackInterface     = "lo"
)

var (
	vrtNetworkCIDR                   = GetVrtNetworkCIDR()
	maxVrtSlotsSize, maxVrtSlotIndex = GetVrtSlotsSizeAndIndex()
	bridgeIP                         net.IP
	once                             sync.Once
)

type Slot struct {
	Key string
	Idx int

	vPeerIp  net.IP
	vrtMask  net.IPMask
	bridgeIp net.IP

	tapIp   net.IP
	tapMask net.IPMask
}

func NewSlot(key string, idx int) (*Slot, error) {
	if idx < firstSlotIndex || idx > maxVrtSlotIndex {
		return nil, fmt.Errorf("slot index %d is out of range [%d, %d]", idx, firstSlotIndex, maxVrtSlotIndex)
	}

	if vrtNetworkCIDR == nil {
		return nil, fmt.Errorf("invaild vrt network CIDR IP")
	}

	vPeerIp, err := netutils.GetIndexedIP(vrtNetworkCIDR, idx*vrtAddressPerSlot)
	if err != nil {
		return nil, fmt.Errorf("failed to get vpeer indexed IP: %w", err)
	}

	vrtCIDR := fmt.Sprintf("%s/%d", vPeerIp.String(), vrtMask)
	_, vrtNet, err := net.ParseCIDR(vrtCIDR)
	if err != nil {
		return nil, fmt.Errorf("failed to parse vrt CIDR: %w", err)
	}

	tapCIDR := fmt.Sprintf("%s/%d", tapIp, tapMask)
	tapIp, tapNet, err := net.ParseCIDR(tapCIDR)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tap CIDR: %w", err)
	}
	// Add bridge ip
	var err1 error
	addBridgeAddr := func() {
		bridgeIp, err := netutils.GetIndexedIP(vrtNetworkCIDR, vrtAddressPerSlot)
		if err != nil {
			err1 = fmt.Errorf("failed to get bridge IP: %w", err)
			return
		}
		ipNet := &net.IPNet{
			IP:   bridgeIp,
			Mask: vrtNet.Mask,
		}
		bridgeLink, err := netlink.LinkByName(bridgeName)
		if err != nil {
			err1 = fmt.Errorf("error finding bridge %s: %w", bridgeName, err)
			return
		}
		err = netlink.AddrAdd(bridgeLink, &netlink.Addr{IPNet: ipNet})
		if err != nil {
			err1 = fmt.Errorf("error add bridge addr: %w", err)
			return
		}
		bridgeIP = bridgeIp
	}
	once.Do(addBridgeAddr)
	if err1 != nil {
		return nil, err1
	}

	slot := &Slot{
		Key: key,
		Idx: idx,

		vPeerIp:  vPeerIp,
		vrtMask:  vrtNet.Mask,
		bridgeIp: bridgeIP,

		tapIp:   tapIp,
		tapMask: tapNet.Mask,
	}
	return slot, nil
}

func (s *Slot) NamespaceID() string {
	return fmt.Sprintf("ns-%d", s.Idx)
}

func (s *Slot) VethName() string {
	return fmt.Sprintf("veth-%d", s.Idx)
}

func (s *Slot) VpeerName() string {
	return fmt.Sprintf("ns-veth-%d", s.Idx)
}

func (s *Slot) BridgeName() string {
	return fmt.Sprintf("bridge-%d", s.Idx)
}

func (s *Slot) VpeerIP() net.IP {
	return s.vPeerIp
}

func (s *Slot) BridgeIP() net.IP {
	return s.bridgeIp
}

func (s *Slot) VpeerIPString() string {
	return s.VpeerIP().String()
}

func (s *Slot) VrtMask() net.IPMask {
	return s.vrtMask
}

func (s *Slot) TapName() string {
	return "tap0"
}

func (s *Slot) TapIP() net.IP {
	return s.tapIp
}

func (s *Slot) TapCIDR() net.IPMask {
	return s.tapMask
}

func (s *Slot) NamespaceIP() string {
	return "192.168.100.21"
}

func (s *Slot) VrtNetworkCIDRString() string {
	return vrtNetworkCIDR.String()
}

func getVrtNetworkCIDR() (*net.IPNet, error) {
	_, subnet, err := net.ParseCIDR(defaultVrtNetworkCIDR)
	return subnet, err
}

func GetVrtNetworkCIDR() *net.IPNet {
	vrtIp, err := getVrtNetworkCIDR()
	if err != nil {
		fmt.Errorf("failed to get vrtNetworkAddr, err is %v", err)
		return nil
	}
	return vrtIp
}

func GetVrtSlotsSizeAndIndex() (slotCount int, maxSlotIndex int) {
	vrtIp, err := getVrtNetworkCIDR()
	if err != nil {
		fmt.Errorf("failed to get vrtNetworkAddr, err is %v", err)
		return invaildSlotSize, invaildSlotSize
	}
	ones, _ := vrtIp.Mask.Size()
	totalIPs := 1 << (32 - ones)

	slotCount = (totalIPs / vrtAddressPerSlot) - vrtAddressPerSlot - 2
	maxSlotIndex = totalIPs - 2

	if slotCount < 0 || maxSlotIndex < firstSlotIndex {
		return invaildSlotSize, invaildSlotSize
	}
	logger.Info("Using network slot size", ulog.F("total_slots", slotCount), ulog.F("max_slot_index", maxSlotIndex))
	return slotCount, maxSlotIndex
}
