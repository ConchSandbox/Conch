package vmm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

type blockingDaemonClient struct {
	release chan struct{}
}

func (c *blockingDaemonClient) BuildStartCmd(*ResourceArgs, bool) (string, error) { return "", nil }
func (c *blockingDaemonClient) CheckDaemonAlive() error {
	<-c.release
	return nil
}
func (c *blockingDaemonClient) PauseVM() error                  { return nil }
func (c *blockingDaemonClient) ResumeVM() error                 { return nil }
func (c *blockingDaemonClient) DeleteVM() error                 { return nil }
func (c *blockingDaemonClient) CreateSnapshot(string) error     { return nil }
func (c *blockingDaemonClient) LoadSnapshot(string, bool) error { return nil }
func (c *blockingDaemonClient) PrepareLaunch(*ResourceArgs) error {
	return nil
}
func (c *blockingDaemonClient) AfterProcessStart() {}
func (c *blockingDaemonClient) WaitForCreateReady(context.Context, <-chan error) error {
	return nil
}
func (c *blockingDaemonClient) WaitForResumeReady(context.Context, <-chan error) error {
	return nil
}
func (c *blockingDaemonClient) Cleanup() {}

func TestWaitForDaemonAliveReturnsProcessExitError(t *testing.T) {
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

	err := process.waitForDaemonAlive(ctx)
	if !errors.Is(err, processErr) {
		t.Fatalf("waitForDaemonAlive() error = %v, want %v", err, processErr)
	}
	if !strings.Contains(err.Error(), "exited before daemon became ready") {
		t.Fatalf("waitForDaemonAlive() error = %q, want early exit context", err.Error())
	}
}
