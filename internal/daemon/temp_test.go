package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureProcessTempDirCreatesMissingDirectory(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "conch", "tmp")
	t.Setenv("TMPDIR", tmpDir)

	if err := ensureProcessTempDir(); err != nil {
		t.Fatalf("ensureProcessTempDir() error = %v", err)
	}
	if info, err := os.Stat(tmpDir); err != nil || !info.IsDir() {
		t.Fatalf("temp directory stat = %#v, error = %v", info, err)
	}
}
