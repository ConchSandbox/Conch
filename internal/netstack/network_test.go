package netstack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCNINetworkNamespacePathIgnoresUnboundPlaceholder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot-2")
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatalf("create placeholder: %v", err)
	}
	if got := cniNetworkNamespacePath(path); got != "" {
		t.Fatalf("cniNetworkNamespacePath() = %q, want empty", got)
	}
}
