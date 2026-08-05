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
[MODIFIED] - Changes made on 2026-08-05 by Team conch: Keep mutable Slot state private to netstack.
*/
package netstack

import (
	"fmt"
	"net"
	"path/filepath"

	netutils "k8s.io/utils/net"
)

const (
	firstSlotID = 2
	maxSlots    = 4000

	defaultTapIP           = "192.168.100.2"
	defaultTapMask         = 24
	networkNamespaceDir    = "/run/conch/netns"
	networkNamespacePrefix = "slot-"
	tapInterfaceName       = "tap0"
	namespaceIPIndex       = 21
)

// slotConfig contains the immutable network addressing shared by Slots in a Pool.
type slotConfig struct {
	tapIP       net.IP
	tapMask     net.IPMask
	namespaceIP net.IP
}

func newSlotConfig(tapIP string, tapMask int) (slotConfig, error) {
	if tapIP == "" {
		tapIP = defaultTapIP
	}
	if tapMask == 0 {
		tapMask = defaultTapMask
	}

	tapCIDR := fmt.Sprintf("%s/%d", tapIP, tapMask)
	parsedTapIP, tapNet, err := net.ParseCIDR(tapCIDR)
	if err != nil {
		return slotConfig{}, fmt.Errorf("failed to parse tap CIDR %q: %w", tapCIDR, err)
	}
	namespaceIP, err := netutils.GetIndexedIP(tapNet, namespaceIPIndex)
	if err != nil {
		return slotConfig{}, fmt.Errorf("failed to derive namespace IP from tap CIDR %q: %w", tapCIDR, err)
	}

	return slotConfig{
		tapIP:       parsedTapIP,
		tapMask:     tapNet.Mask,
		namespaceIP: namespaceIP,
	}, nil
}

// Slot describes one reusable network slot. Callers outside netstack receive
// read-only access; Pool owns all assignment and CNI state transitions.
type Slot struct {
	id int

	sandboxID string
	cniIP     string

	tapIP       net.IP
	tapMask     net.IPMask
	namespaceIP net.IP
}

func newSlot(id int, cfg slotConfig) (*Slot, error) {
	if err := validateSlotID(id); err != nil {
		return nil, err
	}
	if cfg.tapIP == nil || cfg.tapMask == nil || cfg.namespaceIP == nil {
		return nil, fmt.Errorf("slot config is not initialized")
	}

	return &Slot{
		id:          id,
		tapIP:       append(net.IP(nil), cfg.tapIP...),
		tapMask:     append(net.IPMask(nil), cfg.tapMask...),
		namespaceIP: append(net.IP(nil), cfg.namespaceIP...),
	}, nil
}

func validateSlotID(id int) error {
	if id < firstSlotID || id >= firstSlotID+maxSlots {
		return fmt.Errorf("slot ID %d is outside supported range [%d, %d)", id, firstSlotID, firstSlotID+maxSlots)
	}
	return nil
}

func (s *Slot) ID() int {
	return s.id
}

func (s *Slot) namespaceID() string {
	return fmt.Sprintf("%s%d", networkNamespacePrefix, s.id)
}

func (s *Slot) NetNSPath() string {
	return filepath.Join(networkNamespaceDir, s.namespaceID())
}

func (s *Slot) cniContainerID() string {
	return fmt.Sprintf("conch-slot-%d", s.id)
}

func (s *Slot) recordCNIIP(cniIP string) {
	s.cniIP = cniIP
}

func (s *Slot) clearCNIIP() {
	s.cniIP = ""
}

func (s *Slot) assignSandbox(sandboxID string) {
	s.sandboxID = sandboxID
}

func (s *Slot) clearSandboxAssignment() {
	s.sandboxID = ""
}

func (s *Slot) CNIIP() string {
	return s.cniIP
}

func (s *Slot) TapName() string {
	return tapInterfaceName
}

func (s *Slot) tapAddress() net.IP {
	return append(net.IP(nil), s.tapIP...)
}

func (s *Slot) tapMaskValue() net.IPMask {
	return append(net.IPMask(nil), s.tapMask...)
}

func (s *Slot) namespaceAddress() string {
	return s.namespaceIP.String()
}
