package netstack

import (
	"errors"
	"testing"
)

func TestStorageLocalClaimCountsRestoredSlots(t *testing.T) {
	storage := &StorageLocal{
		hostNs:      map[string]struct{}{"ns-2": {}},
		maxSlotSize: 1,
		acquiredNs:  make(map[string]struct{}),
	}
	first := &Slot{Key: "2", Idx: firstSlotIndex}
	second := &Slot{Key: "3", Idx: firstSlotIndex + 1}

	if err := storage.Claim(first); err != nil {
		t.Fatalf("Claim(first) error = %v", err)
	}
	if err := storage.Claim(first); err != nil {
		t.Fatalf("Claim(first) idempotent error = %v", err)
	}
	if _, ok := storage.hostNs["ns-2"]; ok {
		t.Fatal("claimed namespace still tracked as foreign")
	}
	if err := storage.Claim(second); !errors.Is(err, ErrNoAvailableNetworkSlots) {
		t.Fatalf("Claim(second) error = %v, want ErrNoAvailableNetworkSlots", err)
	}

	if err := storage.Release(first); err != nil {
		t.Fatalf("Release(first) error = %v", err)
	}
	if err := storage.Claim(second); err != nil {
		t.Fatalf("Claim(second) after release error = %v", err)
	}
}
