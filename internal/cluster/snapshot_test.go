package cluster

import (
	"math"
	"testing"

	"github.com/prometheus/procfs"

	"github.com/openeuler/Conch/internal/sandbox"
)

func TestResourceSnapshotRejectsInvalidAllocations(t *testing.T) {
	cases := []struct {
		name    string
		records []sandbox.Record
	}{
		{"negative cpu", []sandbox.Record{{ID: "a", State: sandbox.StateReady, VCPUNum: -1}}},
		{"negative memory", []sandbox.Record{{ID: "a", State: sandbox.StateReady, RamMB: -1}}},
		{"cpu sum overflow", []sandbox.Record{{ID: "a", State: sandbox.StateReady, VCPUNum: math.MaxUint32}, {ID: "b", State: sandbox.StateReady, VCPUNum: 1}}},
		{"memory overflow", []sandbox.Record{{ID: "a", State: sandbox.StateReady, RamMB: math.MaxInt64}}},
		{"invalid state", []sandbox.Record{{ID: "a", State: "broken"}}},
		{"empty id", []sandbox.Record{{State: sandbox.StateReady}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := resourceSnapshot(tc.records); err == nil {
				t.Fatal("invalid allocation was accepted")
			}
		})
	}
}

func TestResourceSnapshotKeepsUnconfirmedRecordAllocated(t *testing.T) {
	// A retained UNKNOWN record still owns its reservation: Manager keeps the
	// record exactly until cleanup confirms release.
	snapshot, ids, err := resourceSnapshot([]sandbox.Record{
		{ID: "cleanup-failed", State: sandbox.StateUnknown, VCPUNum: 2, RamMB: 1024},
		{ID: "ready", State: sandbox.StateReady, VCPUNum: 2, RamMB: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "cleanup-failed" || ids[1] != "ready" {
		t.Fatalf("roster lost a failed sandbox: %v", ids)
	}
	if snapshot.AllocatedCpu != 4 || snapshot.AllocatedMemoryBytes != 2*1024*1024*1024 ||
		snapshot.SandboxCount != 1 || snapshot.SandboxStartingCount != 0 || snapshot.PausedSandboxCount != 0 {
		t.Fatalf("failed resources were reported incorrectly: %v", snapshot)
	}
}

// A PAUSED record owns no live runtime: its resources count only into the
// paused_* metrics and never into the running allocation.
func TestResourceSnapshotSeparatesPausedFromRunning(t *testing.T) {
	snapshot, ids, err := resourceSnapshot([]sandbox.Record{
		{ID: "a-paused", State: sandbox.StatePaused, VCPUNum: 2, RamMB: 512},
		{ID: "b-running", State: sandbox.StateReady, VCPUNum: 1, RamMB: 256},
		{ID: "c-creating", State: sandbox.StateCreating, VCPUNum: 1, RamMB: 256},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("roster lost records: %v", ids)
	}
	if snapshot.SandboxCount != 1 || snapshot.SandboxStartingCount != 1 ||
		snapshot.AllocatedCpu != 2 || snapshot.AllocatedMemoryBytes != 512*1024*1024 {
		t.Fatalf("running totals include paused sandbox: %v", snapshot)
	}
	if snapshot.PausedSandboxCount != 1 || snapshot.PausedAllocatedCpu != 2 ||
		snapshot.PausedAllocatedMemoryBytes != 512*1024*1024 {
		t.Fatalf("paused metrics = %v", snapshot)
	}
}

func TestCPUPercentDoesNotDoubleCountGuestTime(t *testing.T) {
	before := procfs.CPUStat{User: 100, Idle: 100, Guest: 100}
	after := procfs.CPUStat{User: 150, Idle: 150, Guest: 150}
	if got := cpuPercent(before, after); got != 50 {
		t.Fatalf("CPU utilization = %d%%, want 50%%", got)
	}
	if got := cpuPercent(after, before); got != 0 {
		t.Fatalf("reset counter utilization = %d%%, want 0%%", got)
	}
}

func TestSchedulerAddressValidation(t *testing.T) {
	for _, addr := range []string{"localhost:9090", "http://localhost:9090", "http://localhost:9090/", "[::1]:9090"} {
		if _, err := schedulerAddress(addr); err != nil {
			t.Errorf("valid endpoint %q rejected: %v", addr, err)
		}
	}
	for _, addr := range []string{"", ":9090", "localhost", "localhost:0", "localhost:65536", "https://localhost:9090", "http://user@localhost:9090", "http://localhost:9090/path", "http://localhost:9090?q=1"} {
		if _, err := schedulerAddress(addr); err == nil {
			t.Errorf("invalid endpoint %q accepted", addr)
		}
	}
}
