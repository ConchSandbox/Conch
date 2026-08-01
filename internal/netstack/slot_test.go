package netstack

import (
	"path/filepath"
	"testing"
)

func TestNewSlotAndCNIState(t *testing.T) {
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
	if slot.CNIIP() != "" {
		t.Fatalf("CNIIP() = %q before setup, want empty", slot.CNIIP())
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
	if slot.CNIIP() != "10.12.0.2" {
		t.Fatalf("CNIIP() = %q, want 10.12.0.2", slot.CNIIP())
	}
	slot.setSlotNetwork("custom-cni-id", &CNIResult{IP: "10.12.0.3/20"}, nil)
	if slot.CNIIP() != "10.12.0.3/20" {
		t.Fatalf("CNIIP() = %q, want 10.12.0.3/20", slot.CNIIP())
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
	if slot.CNIIP() != "" {
		t.Fatalf("CNIIP() after clear = %q, want empty", slot.CNIIP())
	}
	if slot.CNIContainerID() != "conch-slot-2" {
		t.Fatalf("CNIContainerID() after clear = %q, want conch-slot-2", slot.CNIContainerID())
	}
}
