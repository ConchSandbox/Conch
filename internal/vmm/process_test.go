package vmm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/config"
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

func TestIsSandboxSocketName(t *testing.T) {
	for _, name := range []string{
		"0123456789abcdef.sock",
		"0123456789abcdef.sock.serial",
	} {
		if !isSandboxSocketName(name) {
			t.Fatalf("isSandboxSocketName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"sandbox.sock",
		"0123456789abcdef.serial",
		"0123456789abcdef.sock.bak",
		"0123456789ABCDEf.sock",
	} {
		if isSandboxSocketName(name) {
			t.Fatalf("isSandboxSocketName(%q) = true, want false", name)
		}
	}
}

type blockingDaemonClient struct {
	release      chan struct{}
	cleanupCalls atomic.Int32
}

func (c *blockingDaemonClient) BuildStartCmd(*ResourceArgs, bool) (string, error) { return "", nil }
func (c *blockingDaemonClient) CheckAgentAlive(ctx context.Context, processExited <-chan error) error {
	select {
	case <-c.release:
		return nil
	case waitErr, ok := <-processExited:
		if !ok || waitErr == nil {
			return errors.New("vmm process exited before conch-init became ready")
		}
		return errors.Join(errors.New("vmm process exited before conch-init became ready"), waitErr)
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *blockingDaemonClient) PauseVM() error                  { return nil }
func (c *blockingDaemonClient) ResumeVM() error                 { return nil }
func (c *blockingDaemonClient) DeleteVM() error                 { return nil }
func (c *blockingDaemonClient) CreateSnapshot(string) error     { return nil }
func (c *blockingDaemonClient) LoadSnapshot(string, bool) error { return nil }
func (c *blockingDaemonClient) PrepareLaunch(*ResourceArgs, bool) error {
	return nil
}
func (c *blockingDaemonClient) AfterProcessStart() {}
func (c *blockingDaemonClient) WaitForCreateReady(context.Context, <-chan error) error {
	return nil
}
func (c *blockingDaemonClient) WaitForRestoreReady(context.Context, <-chan error) error {
	return nil
}
func (c *blockingDaemonClient) Cleanup() { c.cleanupCalls.Add(1) }

func TestStopIgnoresProcessDoneWhenProcessAlreadyFinished(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for process: %v", err)
	}

	client := &blockingDaemonClient{release: make(chan struct{})}
	process := &Process{
		cmd:        cmd,
		adapter:    client,
		exitSignal: make(chan error, 1),
	}

	if err := process.Stop(); err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}
	if got := client.cleanupCalls.Load(); got != 1 {
		t.Fatalf("Cleanup() calls = %d, want 1", got)
	}
}

func TestWaitForAgentAliveReturnsProcessExitError(t *testing.T) {
	processErr := errors.New("stratovirt exited after creating qmp socket")
	client := &blockingDaemonClient{release: make(chan struct{})}
	process := &Process{
		adapter:    client,
		exitSignal: make(chan error, 1),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	process.exitSignal <- processErr
	close(process.exitSignal)
	t.Cleanup(func() { close(client.release) })

	err := process.waitForAgentAlive(ctx)
	if !errors.Is(err, processErr) {
		t.Fatalf("waitForAgentAlive() error = %v, want %v", err, processErr)
	}
	if !strings.Contains(err.Error(), "exited before conch-init became ready") {
		t.Fatalf("waitForAgentAlive() error = %q, want early exit context", err.Error())
	}
}
