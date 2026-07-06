package vmm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/config"
	"golang.org/x/sys/unix"
)

func TestSandboxSocketPathUsesShortStableName(t *testing.T) {
	oldWorkDir := config.WorkDir
	t.Cleanup(func() { config.WorkDir = oldWorkDir })
	config.WorkDir = filepath.Join(os.TempDir(), "conch-socket-test-"+strings.Repeat("a", 45))
	t.Cleanup(func() { _ = os.RemoveAll(config.WorkDir) })

	sandboxID := "sandbox-" + strings.Repeat("very-long-id-", 12)
	got, err := SandboxSocketPath("x", sandboxID)
	if err != nil {
		t.Fatalf("SandboxSocketPath() error = %v", err)
	}
	if len(got) > unixSocketPathMax {
		t.Fatalf("socket path length = %d, want <= %d: %s", len(got), unixSocketPathMax, got)
	}
	if strings.Contains(got, sandboxID) {
		t.Fatalf("socket path still embeds sandbox id: %s", got)
	}
	again, err := SandboxSocketPath("x", sandboxID)
	if err != nil {
		t.Fatalf("SandboxSocketPath() second error = %v", err)
	}
	if got != again {
		t.Fatalf("SandboxSocketPath() is not stable: %s vs %s", got, again)
	}
}

func TestSandboxSocketPathRejectsTooLongWorkDir(t *testing.T) {
	oldWorkDir := config.WorkDir
	t.Cleanup(func() { config.WorkDir = oldWorkDir })
	config.WorkDir = filepath.Join(os.TempDir(), "conch-socket-test-"+strings.Repeat("a", 110))
	t.Cleanup(func() { _ = os.RemoveAll(config.WorkDir) })

	if _, err := SandboxSocketPath("x", "sandbox"); err == nil {
		t.Fatalf("SandboxSocketPath() error = nil, want path length error")
	}
}

func TestParseEventsFromFdParsesEventStream(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair() error = %v", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	payload := `{"timestamp":0,"source":"vm","event":"created"}` + "\n" +
		`{"timestamp":1,"source":"vm","event":"booted"}` + "\n"
	if _, err := unix.Write(fds[1], []byte(payload)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	events, err := parseEventsFromFd(fds[0], make([]byte, 4096))
	if err != nil {
		t.Fatalf("parseEventsFromFd() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	if events[1].Source != "vm" || events[1].Event != EventBooted {
		t.Fatalf("second event = %#v, want vm/%s", events[1], EventBooted)
	}
}

func TestWaitVmReadyFdReturnsOnBootEvent(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair() error = %v", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		_, _ = unix.Write(fds[1], []byte(`{"timestamp":1,"source":"vm","event":"booted"}`+"\n"))
	}()

	if err := waitVmReadyFd(ctx, fds[0], "vm", EventBooted); err != nil {
		t.Fatalf("waitVmReadyFd() error = %v", err)
	}
}

func TestCreateVmmFdsCleanupRemovesSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "vmm.sock")
	fds, err := createVmmFds(socketPath)
	if err != nil {
		t.Fatalf("createVmmFds() error = %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("expected API socket path to exist: %v", err)
	}

	fds.cleanup()

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket path still exists after cleanup, stat err = %v", err)
	}
}

func TestVmmFdsCleanupNilSafe(t *testing.T) {
	var fds *VmmFds
	fds.cleanup()
}

func TestWaitForVmmSocketWaitsUntilPathExists(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	process := &Process{VmmSocketPath: socketPath}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.WriteFile(socketPath, []byte{}, 0644)
	}()

	if err := process.waitForVmmSocket(ctx); err != nil {
		t.Fatalf("waitForVmmSocket() error = %v", err)
	}
}
