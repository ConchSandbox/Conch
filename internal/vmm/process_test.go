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
		if !sandboxSocketNameRE.MatchString(name) {
			t.Fatalf("sandboxSocketNameRE.MatchString(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"sandbox.sock",
		"0123456789abcdef.serial",
		"0123456789abcdef.sock.bak",
		"0123456789ABCDEf.sock",
	} {
		if sandboxSocketNameRE.MatchString(name) {
			t.Fatalf("sandboxSocketNameRE.MatchString(%q) = true, want false", name)
		}
	}
}

func TestKillStaleVMMProcessIgnoresReusedPID(t *testing.T) {
	binaries := map[string]string{"test-vmm": "/opt/test/vmm"}
	if err := killStaleVMMProcess(os.Getpid(), binaries); err != nil {
		t.Fatalf("killStaleVMMProcess() error = %v, want nil for non-VMM process", err)
	}
}

func TestMatchesConfiguredVMMCommand(t *testing.T) {
	binaries := map[string]string{
		"vmm-a": "/opt/test/vmm-a",
		"vmm-b": "/opt/test/vmm-b",
	}
	tests := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{name: "configured binary", cmdline: "/opt/test/vmm-a\x00--api-socket\x00socket", want: true},
		{name: "second configured binary", cmdline: "/opt/test/vmm-b\x00--restore", want: true},
		{name: "binary only in argument", cmdline: "/usr/bin/wrapper\x00/opt/test/vmm-a", want: false},
		{name: "path prefix", cmdline: "/opt/test/vmm-a.backup\x00--run", want: false},
		{name: "empty command line", cmdline: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesConfiguredVMMCommand([]byte(tt.cmdline), binaries); got != tt.want {
				t.Fatalf("matchesConfiguredVMMCommand(%q) = %t, want %t", tt.cmdline, got, tt.want)
			}
		})
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

type restoreTrackingAdapter struct {
	restoreReadyCalls atomic.Int32
	loadCalls         atomic.Int32
	resumeCalls       atomic.Int32
	checkCalls        atomic.Int32
}

func (a *restoreTrackingAdapter) BuildStartCmd(*ResourceArgs, bool) (string, error) { return "", nil }
func (a *restoreTrackingAdapter) PrepareLaunch(*ResourceArgs, bool) error           { return nil }
func (a *restoreTrackingAdapter) AfterProcessStart()                                {}
func (a *restoreTrackingAdapter) WaitForCreateReady(context.Context, <-chan error) error {
	return nil
}
func (a *restoreTrackingAdapter) WaitForRestoreReady(context.Context, <-chan error) error {
	a.restoreReadyCalls.Add(1)
	return nil
}
func (a *restoreTrackingAdapter) CheckAgentAlive(context.Context, <-chan error) error {
	a.checkCalls.Add(1)
	return nil
}
func (a *restoreTrackingAdapter) PauseVM() error              { return nil }
func (a *restoreTrackingAdapter) ResumeVM() error             { a.resumeCalls.Add(1); return nil }
func (a *restoreTrackingAdapter) DeleteVM() error             { return nil }
func (a *restoreTrackingAdapter) CreateSnapshot(string) error { return nil }
func (a *restoreTrackingAdapter) LoadSnapshot(string, bool) error {
	a.loadCalls.Add(1)
	return nil
}
func (a *restoreTrackingAdapter) Cleanup() {}

func TestRestoreUsesBackendSpecificResumePolicy(t *testing.T) {
	for _, tt := range []struct {
		name            string
		vmmName         string
		wantResumeCalls int32
	}{
		{name: "stratovirt relies on status check", vmmName: StratovirtName, wantResumeCalls: 0},
		{name: "cloud hypervisor resumes explicitly", vmmName: CloudHypervisorName, wantResumeCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &restoreTrackingAdapter{}
			process := &Process{
				cmd:        exec.Command("true"),
				vmmName:    tt.vmmName,
				SandboxId:  "sandbox-test",
				adapter:    adapter,
				exitSignal: make(chan error, 1),
			}

			if err := process.Restore(context.Background(), "/tmp/snapshot"); err != nil {
				t.Fatalf("Restore() error = %v", err)
			}
			if got := adapter.resumeCalls.Load(); got != tt.wantResumeCalls {
				t.Fatalf("ResumeVM() calls = %d, want %d", got, tt.wantResumeCalls)
			}
			if got := adapter.restoreReadyCalls.Load(); got != 1 {
				t.Fatalf("WaitForRestoreReady() calls = %d, want 1", got)
			}
			if got := adapter.loadCalls.Load(); got != 1 {
				t.Fatalf("LoadSnapshot() calls = %d, want 1", got)
			}
			if got := adapter.checkCalls.Load(); got != 1 {
				t.Fatalf("CheckAgentAlive() calls = %d, want 1", got)
			}
		})
	}
}

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
