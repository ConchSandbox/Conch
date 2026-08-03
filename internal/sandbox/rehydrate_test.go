package sandbox

import (
	"testing"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/netstack"
)

func TestRehydratedNetworkCleanupIsRegisteredOnlyAfterSlotRestore(t *testing.T) {
	sb, err := attachSandboxFromRecord(state.SandboxRecord{
		SandboxID:     "sandbox-a",
		NetworkSlotID: 2,
		VMMName:       "cloud-hypervisor",
		VMMSocketPath: "/tmp/conch-test-vmm.sock",
	})
	if err != nil {
		t.Fatalf("attachSandboxFromRecord() error = %v", err)
	}
	if got := len(sb.cleanup.cleanup); got != 1 {
		t.Fatalf("cleanup functions after attach = %d, want only file cleanup", got)
	}

	registerRehydratedNetworkCleanup(sb, &netstack.Pool{})
	if got := len(sb.cleanup.cleanup); got != 2 {
		t.Fatalf("cleanup functions after network restore = %d, want network cleanup registered", got)
	}
}
