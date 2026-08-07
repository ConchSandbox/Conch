package vmm

import (
	"testing"

	"github.com/openeuler/Conch/internal/vmm/stratovirt"
)

func TestNewVmmAdapterCreatesStratovirtClient(t *testing.T) {
	client, err := newVmmAdapter("stratovirt", "/tmp/qmp.sock", "/opt/vmm/stratovirt")
	if err != nil {
		t.Fatalf("newVmmAdapter() error = %v", err)
	}
	if _, ok := client.(*stratovirt.StratovirtClient); !ok {
		t.Fatalf("client type = %T, want *stratovirt.StratovirtClient", client)
	}
}

func TestNewVmmAdapterRequiresConfiguredBinary(t *testing.T) {
	_, err := newVmmAdapter("stratovirt", "/tmp/qmp.sock", "")
	if err == nil {
		t.Fatal("newVmmAdapter() error = nil, want missing binary error")
	}
}
