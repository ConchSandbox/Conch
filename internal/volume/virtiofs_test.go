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
	if err := backend.Cleanup(sandboxID, []Device{{Exited: done}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("runtime directory remains after Cleanup: %v", err)
	}
}

func TestCleanupIsIdempotentWithoutPreparedVolumes(t *testing.T) {
	backend := &virtiofsBackend{runtimeDir: t.TempDir()}
	for range 3 {
		if err := backend.Cleanup("sandbox-a", nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCleanupRetainsDirectoryOnReadFailure(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "sandbox-a")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	volumeDir := filepath.Join(runtimeDir, volumeDirName)
	if err := os.WriteFile(volumeDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	backend := &virtiofsBackend{runtimeDir: root}
	if err := backend.Cleanup("sandbox-a", nil); err == nil {
		t.Fatal("ignored unreadable volume directory")
	}
	if _, err := os.Stat(runtimeDir); err != nil {
		t.Fatal("lost recovery directory")
	}
	if err := os.Remove(volumeDir); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := backend.Cleanup("sandbox-a", nil); err != nil {
			t.Fatal(err)
		}
	}
}
