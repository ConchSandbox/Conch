package netstack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectNetworkNamespace(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing")
	mounted, missing, err := inspectNetworkNamespace(missingPath)
	if err != nil || mounted || !missing {
		t.Fatalf("inspectNetworkNamespace(missing) = (%v, %v, %v), want (false, true, nil)", mounted, missing, err)
	}

	path := filepath.Join(t.TempDir(), "slot-2")
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatalf("create placeholder: %v", err)
	}
	mounted, missing, err = inspectNetworkNamespace(path)
	if err != nil || mounted || missing {
		t.Fatalf("inspectNetworkNamespace(placeholder) = (%v, %v, %v), want (false, false, nil)", mounted, missing, err)
	}
}
