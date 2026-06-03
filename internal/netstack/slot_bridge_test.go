package netstack

import (
	"net"
	"path/filepath"
	"testing"
)

func withBridgeLayout(t *testing.T, bridgeCount int) {
	t.Helper()
	oldConfiguredBridgeCount := configuredBridgeCount
	oldEffectiveBridgeCount := effectiveBridgeCount
	oldMaxVrtSlotsSize := maxVrtSlotsSize
	oldMaxVrtSlotIndex := maxVrtSlotIndex
	oldBridgeLayoutReady := bridgeLayoutReady
	t.Cleanup(func() {
		configuredBridgeCount = oldConfiguredBridgeCount
		effectiveBridgeCount = oldEffectiveBridgeCount
		maxVrtSlotsSize = oldMaxVrtSlotsSize
		maxVrtSlotIndex = oldMaxVrtSlotIndex
		bridgeLayoutReady = oldBridgeLayoutReady
	})
	if err := initConfigureBridgeLayout(bridgeCount); err != nil {
		t.Fatalf("initConfigureBridgeLayout(%d): %v", bridgeCount, err)
	}
}

func TestRoundUpToPowerOfTwo(t *testing.T) {
	tests := map[int]int{
		0: 1,
		1: 1,
		2: 2,
		3: 4,
		4: 4,
		5: 8,
	}
	for in, want := range tests {
		if got := roundUpToPowerOfTwo(in); got != want {
			t.Fatalf("roundUpToPowerOfTwo(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestBridgeLayoutMath(t *testing.T) {
	withBridgeLayout(t, 3)

	if configuredBridgeCount != 3 {
		t.Fatalf("configuredBridgeCount = %d, want 3", configuredBridgeCount)
	}
	if effectiveBridgeCount != 4 {
		t.Fatalf("effectiveBridgeCount = %d, want 4", effectiveBridgeCount)
	}
	if got := getBridgeName(0); got != "conch_bridge_0" {
		t.Fatalf("getBridgeName(0) = %q, want conch_bridge_0", got)
	}
	if got := getBridgePrefix(); got != 22 {
		t.Fatalf("getBridgePrefix() = %d, want 22", got)
	}
	if got := getSlotsPerBridge(); got != 1021 {
		t.Fatalf("getSlotsPerBridge() = %d, want 1021", got)
	}
	subnet, err := getBridgeSubnet(1)
	if err != nil {
		t.Fatalf("getBridgeSubnet(1): %v", err)
	}
	if subnet.String() != "10.12.4.0/22" {
		t.Fatalf("getBridgeSubnet(1) = %s, want 10.12.4.0/22", subnet.String())
	}
}

func TestSingleBridgeUsesLegacyName(t *testing.T) {
	withBridgeLayout(t, 1)

	if got := getBridgeName(0); got != legacyBridgeName {
		t.Fatalf("getBridgeName(0) = %q, want %q", got, legacyBridgeName)
	}
}

func TestNewSlotAndCNIState(t *testing.T) {
	withBridgeLayout(t, 1)
	oldTapIP := configuredTapIP
	oldTapMask := configuredTapMask
	t.Cleanup(func() {
		configuredTapIP = oldTapIP
		configuredTapMask = oldTapMask
	})
	if err := configureTapNetwork(defaultTapIP, defaultTapMask); err != nil {
		t.Fatalf("configureTapNetwork(): %v", err)
	}

	slot, err := NewSlot("2", firstSlotIndex)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	if slot.NamespaceID() != "ns-2" {
		t.Fatalf("NamespaceID() = %q, want ns-2", slot.NamespaceID())
	}
	if slot.NetNSPath() != filepath.Join(netNamespacesDir, "ns-2") {
		t.Fatalf("NetNSPath() = %q", slot.NetNSPath())
	}
	if slot.CNIContainerID() != "conch-slot-2" {
		t.Fatalf("CNIContainerID() = %q, want conch-slot-2", slot.CNIContainerID())
	}
	slot.setNetNSPath("/tmp/ns-2")
	if slot.NetNSPath() != "/tmp/ns-2" {
		t.Fatalf("NetNSPath() after set = %q, want /tmp/ns-2", slot.NetNSPath())
	}

	result := &CNIResult{IP: "10.12.0.2"}
	slot.setSlotNetwork("custom-cni-id", result, nil)
	if slot.CNIContainerID() != "custom-cni-id" {
		t.Fatalf("CNIContainerID() = %q, want custom-cni-id", slot.CNIContainerID())
	}
	if slot.CNIResult() != result {
		t.Fatalf("CNIResult() did not return stored result")
	}
	if !slot.VpeerIP().Equal(net.ParseIP("10.12.0.2")) {
		t.Fatalf("VpeerIP() = %v, want 10.12.0.2", slot.VpeerIP())
	}

	slot.assignSandbox("sandbox-a")
	if slot.SandboxID() != "sandbox-a" {
		t.Fatalf("SandboxID() = %q, want sandbox-a", slot.SandboxID())
	}
	slot.clearSandboxAssignment()
	if slot.SandboxID() != "" {
		t.Fatalf("SandboxID() after clear = %q, want empty", slot.SandboxID())
	}
	slot.clearSlotNetwork()
	if slot.CNIResult() != nil {
		t.Fatalf("CNIResult() after clear = %#v, want nil", slot.CNIResult())
	}
	if slot.CNIContainerID() != "conch-slot-2" {
		t.Fatalf("CNIContainerID() after clear = %q, want conch-slot-2", slot.CNIContainerID())
	}
}
