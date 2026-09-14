package guestd

import (
	"reflect"
	"testing"
)

func TestOrderedPmemDevicesNumericOrder(t *testing.T) {
	entries := []string{
		"/dev/pmem9",
		"/dev/pmem2",
		"/dev/pmem10",
		"/dev/pmem0p1",
		"/dev/pmem01",
		"/dev/pmemx",
		"/dev/pmem1",
		"/dev/pmem0",
	}
	want := []string{
		"/dev/pmem0",
		"/dev/pmem1",
		"/dev/pmem2",
		"/dev/pmem9",
		"/dev/pmem10",
	}

	got := orderedPmemDevices(entries)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderedPmemDevices() = %v, want %v", got, want)
	}
	if entries[0] != "/dev/pmem9" {
		t.Fatalf("orderedPmemDevices() mutated input: %v", entries)
	}
}
