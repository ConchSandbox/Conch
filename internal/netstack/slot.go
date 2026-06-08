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
[MODIFIED] - Changes made on 2026-05-13 by Team conch: Add slot-owned CNI state for reusable sandbox network slots.
*/
package netstack

import (
	"fmt"
	"net"
	"path/filepath"

	netutils "k8s.io/utils/net"
)

const (
	defaultTapIP      = "192.168.100.2"
	defaultTapMask    = 24
	vrtMask           = 20
	invaildSlotSize   = 0
	vrtAddressPerSlot = 1
	// Index 1 is reserved for the bridge IP, so sandbox slots start from index 2.
	firstSlotIndex    = 2
	tapInterfaceName  = "tap0"
	loopbackInterface = "lo"
	namespaceIPIndex  = 21
)

var (
	configuredTapIP   = defaultTapIP
	configuredTapMask = defaultTapMask
)

type Slot struct {
	Key string
	Idx int

	sandboxID string
	cniID     string
	netnsPath string
	cniResult *CNIResult
	cniOpts   []NamespaceOpts

	bridgeOrdinal int
	vPeerIp       net.IP
	vrtMask       net.IPMask
	bridgeIp      net.IP

	tapIp       net.IP
	tapMask     net.IPMask
	namespaceIP net.IP
}

func NewSlot(key string, idx int) (*Slot, error) {
	if !bridgeLayoutReady {
		return nil, fmt.Errorf("bridge layout is not configured")
	}

	if idx < firstSlotIndex || idx > maxVrtSlotIndex {
		return nil, fmt.Errorf("slot index %d is out of range [%d, %d]", idx, firstSlotIndex, maxVrtSlotIndex)
	}

	if vrtNetworkCIDR == nil {
		return nil, fmt.Errorf("invaild vrt network CIDR IP")
	}

	slotNumber := idx - firstSlotIndex
	slotsPerBridge := getSlotsPerBridge()
	if slotsPerBridge == invaildSlotSize {
		return nil, fmt.Errorf("invalid bridge slot size")
	}

	// Round-robin bridges first, then advance the host offset inside that bridge.
	bridgeOrdinal := slotNumber % configuredBridgeCount
	bridgeLocalIndex := slotNumber / configuredBridgeCount
	hostOffset := bridgeHostOffset(bridgeLocalIndex)

	bridgeNet, err := getBridgeSubnet(bridgeOrdinal)
	if err != nil {
		return nil, fmt.Errorf("failed to get bridge subnet: %w", err)
	}

	vPeerIp, err := netutils.GetIndexedIP(bridgeNet, hostOffset)
	if err != nil {
		return nil, fmt.Errorf("failed to get vpeer indexed IP: %w", err)
	}

	vrtCIDR := fmt.Sprintf("%s/%d", vPeerIp.String(), vrtMask)
	_, vrtNet, err := net.ParseCIDR(vrtCIDR)
	if err != nil {
		return nil, fmt.Errorf("failed to parse vrt CIDR: %w", err)
	}

	tapCIDR := fmt.Sprintf("%s/%d", configuredTapIP, configuredTapMask)
	tapIP, tapNet, err := net.ParseCIDR(tapCIDR)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tap CIDR: %w", err)
	}
	namespaceIP, err := netutils.GetIndexedIP(tapNet, namespaceIPIndex)
	if err != nil {
		return nil, fmt.Errorf("failed to derive namespace IP from tap CIDR: %w", err)
	}

	bridgeIP, err := netutils.GetIndexedIP(bridgeNet, vrtAddressPerSlot)
	if err != nil {
		return nil, fmt.Errorf("failed to get bridge IP: %w", err)
	}

	slot := &Slot{
		Key:           key,
		Idx:           idx,
		bridgeOrdinal: bridgeOrdinal,

		vPeerIp:  vPeerIp,
		vrtMask:  vrtNet.Mask,
		bridgeIp: bridgeIP,

		tapIp:       tapIP,
		tapMask:     tapNet.Mask,
		namespaceIP: namespaceIP,
	}
	return slot, nil
}

func configureTapNetwork(tapIP string, tapMask int) error {
	if tapIP == "" {
		tapIP = defaultTapIP
	}
	if tapMask == 0 {
		tapMask = defaultTapMask
	}

	tapCIDR := fmt.Sprintf("%s/%d", tapIP, tapMask)
	_, tapNet, err := net.ParseCIDR(tapCIDR)
	if err != nil {
		return fmt.Errorf("failed to parse tap CIDR %q: %w", tapCIDR, err)
	}
	if _, err := netutils.GetIndexedIP(tapNet, namespaceIPIndex); err != nil {
		return fmt.Errorf("failed to derive namespace IP from tap CIDR %q: %w", tapCIDR, err)
	}

	configuredTapIP = tapIP
	configuredTapMask = tapMask
	return nil
}

func (s *Slot) NamespaceID() string {
	return fmt.Sprintf("ns-%d", s.Idx)
}

func (s *Slot) NetNSPath() string {
	if s.netnsPath != "" {
		return s.netnsPath
	}
	return filepath.Join(netNamespacesDir, s.NamespaceID())
}

func (s *Slot) setNetNSPath(netnsPath string) {
	s.netnsPath = netnsPath
}

func (s *Slot) CNIContainerID() string {
	if s.cniID != "" {
		return s.cniID
	}
	return fmt.Sprintf("conch-slot-%s", s.Key)
}

func (s *Slot) setSlotNetwork(cniID string, cniResult *CNIResult, cniOpts []NamespaceOpts) {
	s.cniID = cniID
	s.cniResult = cniResult
	s.cniOpts = cniOpts
	if cniResult != nil && cniResult.IP != "" {
		s.vPeerIp = parseCNIResultIP(cniResult.IP)
	}
}

func (s *Slot) assignSandbox(sandboxID string) {
	s.sandboxID = sandboxID
}

func (s *Slot) clearSandboxAssignment() {
	s.sandboxID = ""
}

func (s *Slot) clearSlotNetwork() {
	s.cniID = ""
	s.cniResult = nil
	s.cniOpts = nil
}

func (s *Slot) SandboxID() string {
	return s.sandboxID
}

func (s *Slot) CNIResult() *CNIResult {
	return s.cniResult
}

func (s *Slot) VethName() string {
	return fmt.Sprintf("veth-%d", s.Idx)
}

func (s *Slot) VpeerName() string {
	return fmt.Sprintf("ns-veth-%d", s.Idx)
}

func (s *Slot) BridgeName() string {
	return getBridgeName(s.bridgeOrdinal)
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
	return tapInterfaceName
}

func (s *Slot) TapIP() net.IP {
	return s.tapIp
}

func (s *Slot) TapCIDR() net.IPMask {
	return s.tapMask
}

func (s *Slot) NamespaceIP() string {
	return s.namespaceIP.String()
}

func (s *Slot) VrtNetworkCIDRString() string {
	return vrtNetworkCIDR.String()
}

func bridgeHostOffset(bridgeLocalIndex int) int {
	// Offset 0 is the subnet address, 1 is reserved for the bridge IP, so slot
	// addresses start at host offset 2 inside each bridge-local subnet.
	return bridgeLocalIndex + 2
}
