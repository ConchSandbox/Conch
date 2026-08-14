package volume

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupWaitsForVirtiofsProcessExit(t *testing.T) {
	const sandboxID = "sandbox-a"
	runtimeRoot := t.TempDir()
	runtimeDir := filepath.Join(runtimeRoot, sandboxID)
	if err := os.MkdirAll(filepath.Join(runtimeDir, volumeDirName), 0o755); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	done := make(chan struct{})
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-waited
		select {
		case <-done:
		default:
			close(done)
		}
	}()

	backend := &virtiofsBackend{runtimeDir: runtimeRoot}
	backend.procs.Store(sandboxID, cmd)
	cleanupResult := make(chan error, 1)
	go func() {
		cleanupResult <- backend.Cleanup(sandboxID, []Device{{Exited: done}})
	}()

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("virtiofs helper process was not killed")
	}
	select {
	case err := <-cleanupResult:
		t.Fatalf("Cleanup returned before process exit notification: %v", err)
	default:
	}
	if _, err := os.Stat(runtimeDir); err != nil {
		t.Fatalf("runtime directory removed before process exit notification: %v", err)
	}

	close(done)
	select {
	case err := <-cleanupResult:
		if err != nil {
			t.Fatalf("Cleanup() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cleanup did not finish after process exit notification")
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("runtime directory remains after Cleanup: %v", err)
	}
}
