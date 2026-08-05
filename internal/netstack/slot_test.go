package netstack

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestNewSlotAndCNIState(t *testing.T) {
	cfg, err := newSlotConfig("", 0)
	if err != nil {
		t.Fatalf("newSlotConfig(): %v", err)
	}
	slot, err := newSlot(firstSlotID, cfg)
	if err != nil {
		t.Fatalf("newSlot(): %v", err)
	}
	if slot.namespaceID() != "slot-2" {
		t.Fatalf("namespaceID() = %q, want slot-2", slot.namespaceID())
	}
	if slot.ID() != 2 {
		t.Fatalf("ID() = %d, want 2", slot.ID())
	}
	if slot.NetNSPath() != filepath.Join(networkNamespaceDir, "slot-2") {
		t.Fatalf("NetNSPath() = %q", slot.NetNSPath())
	}
	if slot.cniContainerID() != "conch-slot-2" {
		t.Fatalf("cniContainerID() = %q, want conch-slot-2", slot.cniContainerID())
	}
	if slot.CNIIP() != "" {
		t.Fatalf("CNIIP() = %q before setup, want empty", slot.CNIIP())
	}
	slot.recordCNIIP("10.12.0.2")
	if slot.CNIIP() != "10.12.0.2" {
		t.Fatalf("CNIIP() = %q, want 10.12.0.2", slot.CNIIP())
	}
	slot.recordCNIIP("10.12.0.3/20")
	if slot.CNIIP() != "10.12.0.3/20" {
		t.Fatalf("CNIIP() = %q, want 10.12.0.3/20", slot.CNIIP())
	}

	slot.assignSandbox("sandbox-a")
	if slot.sandboxID != "sandbox-a" {
		t.Fatalf("sandboxID = %q, want sandbox-a", slot.sandboxID)
	}
	slot.clearSandboxAssignment()
	if slot.sandboxID != "" {
		t.Fatalf("sandboxID after clear = %q, want empty", slot.sandboxID)
	}
	slot.clearCNIIP()
	if slot.CNIIP() != "" {
		t.Fatalf("CNIIP() after clear = %q, want empty", slot.CNIIP())
	}
	if slot.cniContainerID() != "conch-slot-2" {
		t.Fatalf("cniContainerID() after clear = %q, want conch-slot-2", slot.cniContainerID())
	}
}

func TestNewSlotRejectsOutOfRangeID(t *testing.T) {
	cfg, err := newSlotConfig("", 0)
	if err != nil {
		t.Fatalf("newSlotConfig(): %v", err)
	}
	for _, id := range []int{firstSlotID - 1, firstSlotID + maxSlots} {
		t.Run(fmt.Sprintf("id_%d", id), func(t *testing.T) {
			if _, err := newSlot(id, cfg); err == nil {
				t.Fatalf("newSlot(%d) error = nil", id)
			}
		})
	}
}

func TestNewSlotConfigRejectsInvalidTapNetwork(t *testing.T) {
	if _, err := newSlotConfig("not-an-ip", 24); err == nil {
		t.Fatal("newSlotConfig() error = nil, want invalid tap IP error")
	}
}
