package vmm

import (
	"testing"

	"github.com/openeuler/Conch/internal/vmm/stratovirt"
)

func TestNewVmmAdapterCreatesStratovirtClient(t *testing.T) {
	client, err := newVmmAdapter("stratovirt", "/tmp/qmp.sock")
	if err != nil {
		t.Fatalf("newVmmAdapter() error = %v", err)
	}
	if _, ok := client.(*stratovirt.StratovirtClient); !ok {
		t.Fatalf("client type = %T, want *stratovirt.StratovirtClient", client)
	}
}
