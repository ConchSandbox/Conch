package netstack

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNewSlotAndCNIState(t *testing.T) {
	namespaceDir := t.TempDir()
	cfg := newSlotConfig(namespaceDir)
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
	if slot.NetNSPath() != filepath.Join(namespaceDir, "slot-2") {
		t.Fatalf("NetNSPath() = %q", slot.NetNSPath())
	}
	if slot.cniContainerID() != "conch-slot-2" {
		t.Fatalf("cniContainerID() = %q, want conch-slot-2", slot.cniContainerID())
	}
	if slot.CNIIP() != "" {
		t.Fatalf("CNIIP() = %q before setup, want empty", slot.CNIIP())
	}
	slot.recordCNIResult(CNIResult{IP: "10.12.0.2"})
	if slot.CNIIP() != "10.12.0.2" {
		t.Fatalf("CNIIP() = %q, want 10.12.0.2", slot.CNIIP())
	}
	wantDNS := DNSConfig{Nameservers: []string{"10.0.0.53"}}
	slot.recordCNIResult(CNIResult{IP: "10.12.0.3/20", DNS: wantDNS})
	if slot.CNIIP() != "10.12.0.3/20" {
		t.Fatalf("CNIIP() = %q, want 10.12.0.3/20", slot.CNIIP())
	}

	slot.assignSandbox("sandbox-a")
	if slot.sandboxID != "sandbox-a" {
		t.Fatalf("sandboxID = %q, want sandbox-a", slot.sandboxID)
	}
	if got := slot.GuestNetworkConfig().DNS; !reflect.DeepEqual(got, wantDNS) {
		t.Fatalf("guest DNS = %#v, want CNI DNS %#v", got, wantDNS)
	}
	slot.clearSandboxAssignment()
	if slot.sandboxID != "" {
		t.Fatalf("sandboxID after clear = %q, want empty", slot.sandboxID)
	}
	slot.clearCNIResult()
	if slot.CNIIP() != "" {
		t.Fatalf("CNIIP() after clear = %q, want empty", slot.CNIIP())
	}
	if slot.cniContainerID() != "conch-slot-2" {
		t.Fatalf("cniContainerID() after clear = %q, want conch-slot-2", slot.cniContainerID())
	}
}

func TestNewSlotRejectsOutOfRangeID(t *testing.T) {
	cfg := newSlotConfig(t.TempDir())
	for _, id := range []int{firstSlotID - 1, firstSlotID + maxSlots} {
		t.Run(fmt.Sprintf("id_%d", id), func(t *testing.T) {
			if _, err := newSlot(id, cfg); err == nil {
				t.Fatalf("newSlot(%d) error = nil", id)
			}
		})
	}
}

func TestNewSlotConfigUsesInternalGuestNetwork(t *testing.T) {
	cfg := newSlotConfig(t.TempDir())
	if got := cfg.tapIP.String(); got != guestGatewayIP {
		t.Fatalf("tap IP = %q, want %q", got, guestGatewayIP)
	}
	if got := cfg.namespaceIP.String(); got != guestIP {
		t.Fatalf("guest IP = %q, want %q", got, guestIP)
	}
	ones, bits := cfg.tapMask.Size()
	if ones != guestPrefixLength || bits != 32 {
		t.Fatalf("tap mask = (%d, %d), want (%d, 32)", ones, bits, guestPrefixLength)
	}
}
