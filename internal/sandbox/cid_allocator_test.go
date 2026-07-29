package sandbox

import (
	"strings"
	"testing"
)

func TestReserveCIDIsIdempotentAndPreservesOwnership(t *testing.T) {
	allocator := NewCIDAllocatorInDir(t.TempDir())

	if err := allocator.ReserveCID("sandbox-a", 42); err != nil {
		t.Fatalf("ReserveCID(first) error = %v", err)
	}
	if err := allocator.ReserveCID("sandbox-a", 42); err != nil {
		t.Fatalf("ReserveCID(idempotent) error = %v", err)
	}
	if got, ok := allocator.GetCID("sandbox-a"); !ok || got != 42 {
		t.Fatalf("GetCID(sandbox-a) = (%d, %v), want (42, true)", got, ok)
	}

	if err := allocator.ReserveCID("sandbox-b", 42); err == nil {
		t.Fatal("ReserveCID(conflicting owner) error = nil")
	} else if !strings.Contains(err.Error(), "sandbox-a") {
		t.Fatalf("ReserveCID(conflicting owner) error = %v, want existing owner", err)
	}
	if err := allocator.ReserveCID("sandbox-a", 43); err == nil {
		t.Fatal("ReserveCID(changed cid) error = nil")
	}
	if _, ok := allocator.GetCID("sandbox-b"); ok {
		t.Fatal("conflicting owner was recorded")
	}
}
