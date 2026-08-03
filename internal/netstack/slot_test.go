package netstack

import (
	"fmt"
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

	slot, err := NewSlot(firstSlotID)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	if slot.NamespaceID() != "slot-2" {
		t.Fatalf("NamespaceID() = %q, want slot-2", slot.NamespaceID())
	}
	if slot.ID != 2 {
		t.Fatalf("ID = %d, want 2", slot.ID)
	}
	if slot.NetNSPath() != filepath.Join(netNamespacesDir, "slot-2") {
		t.Fatalf("NetNSPath() = %q", slot.NetNSPath())
	}
	if slot.CNIContainerID() != "conch-slot-2" {
		t.Fatalf("CNIContainerID() = %q, want conch-slot-2", slot.CNIContainerID())
	}
	if slot.CNIIP() != "" {
		t.Fatalf("CNIIP() = %q before setup, want empty", slot.CNIIP())
	}
	result := &CNIResult{IP: "10.12.0.2"}
	slot.setCNIResult(result)
	if slot.CNIResult() != result {
		t.Fatalf("CNIResult() did not return stored result")
	}
	if slot.CNIIP() != "10.12.0.2" {
		t.Fatalf("CNIIP() = %q, want 10.12.0.2", slot.CNIIP())
	}
	slot.setCNIResult(&CNIResult{IP: "10.12.0.3/20"})
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
	slot.clearCNIResult()
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

func TestNewSlotRejectsOutOfRangeID(t *testing.T) {
	for _, id := range []int{firstSlotID - 1, firstSlotID + maxSlots} {
		t.Run(fmt.Sprintf("id_%d", id), func(t *testing.T) {
			if _, err := NewSlot(id); err == nil {
				t.Fatalf("NewSlot(%d) error = nil", id)
			}
		})
	}
}
