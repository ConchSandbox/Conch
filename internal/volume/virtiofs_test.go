package volume

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	virtiofsHelperEnv    = "CONCH_TEST_VIRTIOFSD_HELPER"
	virtiofsHelperMode   = "CONCH_TEST_VIRTIOFSD_MODE"
	virtiofsHelperSocket = "CONCH_TEST_VIRTIOFSD_SOCKET"
)

func TestVirtiofsdHelperProcess(t *testing.T) {
	if os.Getenv(virtiofsHelperEnv) != "1" {
		return
	}
	mode := os.Getenv(virtiofsHelperMode)
	socket := os.Getenv(virtiofsHelperSocket)
	switch mode {
	case "exit-before-socket":
		os.Exit(7)
	case "never-create-socket":
		time.Sleep(time.Hour)
	case "socket-and-wait":
		listener, err := net.Listen("unix", socket)
		if err != nil {
			os.Exit(21)
		}
		_, _ = listener.Accept()
		os.Exit(22)
	case "socket-and-exit":
		listener, err := net.Listen("unix", socket)
		if err != nil {
			os.Exit(23)
		}
		time.Sleep(5 * time.Millisecond)
		runtime.KeepAlive(listener)
		os.Exit(9)
	default:
		os.Exit(24)
	}
}

func virtiofsHelperCommand(socket, mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestVirtiofsdHelperProcess$")
	cmd.Env = append(os.Environ(),
		virtiofsHelperEnv+"=1",
		virtiofsHelperMode+"="+mode,
		virtiofsHelperSocket+"="+socket,
	)
	return cmd
}

func TestWaitUnixSocketReturnsWhenSocketExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virtiofs.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	if err := waitUnixSocket(path, time.Second, nil); err != nil {
		t.Fatalf("waitUnixSocket() error = %v", err)
	}
}

func TestWaitUnixSocketReturnsImmediatelyWhenProcessExits(t *testing.T) {
	done := make(chan struct{})
	close(done)
	started := time.Now()
	err := waitUnixSocket(filepath.Join(t.TempDir(), "missing.sock"), time.Second, done)
	if err == nil || !strings.Contains(err.Error(), "exited before socket") {
		t.Fatalf("waitUnixSocket() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("waitUnixSocket() took %s after process exit", elapsed)
	}
}

func TestStartVirtiofsProcessStartFailureDoesNotRegisterProcess(t *testing.T) {
	backend := &virtiofsBackend{}
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing-virtiofsd"))

	process, err := backend.startVirtiofsProcess("sandbox-a", cmd, filepath.Join(t.TempDir(), "virtiofs.sock"), time.Second)
	if err == nil || process != nil {
		t.Fatalf("startVirtiofsProcess() process=%p error=%v, want start failure", process, err)
	}
	if cmd.Process != nil {
		t.Fatalf("failed command unexpectedly has process pid %d", cmd.Process.Pid)
	}
	if _, ok := backend.procs.Load("sandbox-a"); ok {
		t.Fatal("failed command was registered in backend map")
	}
}

func TestStartVirtiofsProcessReapsExitBeforeSocket(t *testing.T) {
	backend := &virtiofsBackend{}
	socket := filepath.Join(t.TempDir(), "virtiofs.sock")
	cmd := virtiofsHelperCommand(socket, "exit-before-socket")

	process, err := backend.startVirtiofsProcess("sandbox-a", cmd, socket, time.Second)
	if err == nil || process != nil {
		t.Fatalf("startVirtiofsProcess() process=%p error=%v, want startup exit", process, err)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 7 {
		t.Fatalf("helper ProcessState = %#v, want reaped exit code 7", cmd.ProcessState)
	}
	if _, ok := backend.procs.Load("sandbox-a"); ok {
		t.Fatal("exited startup process remains in backend map")
	}
}

func TestStartVirtiofsProcessTimeoutKillsAndReapsChild(t *testing.T) {
	backend := &virtiofsBackend{}
	socket := filepath.Join(t.TempDir(), "virtiofs.sock")
	cmd := virtiofsHelperCommand(socket, "never-create-socket")

	process, err := backend.startVirtiofsProcess("sandbox-a", cmd, socket, 30*time.Millisecond)
	if err == nil || process != nil {
		t.Fatalf("startVirtiofsProcess() process=%p error=%v, want socket timeout", process, err)
	}
	assertReapedBySIGKILL(t, cmd)
	if _, ok := backend.procs.Load("sandbox-a"); ok {
		t.Fatal("timed-out process remains in backend map")
	}
}

func TestPreparedProcessExitRemainsObservableAndCleanupUsesExactOwner(t *testing.T) {
	backend := &virtiofsBackend{runtimeDir: t.TempDir()}
	socket := filepath.Join(t.TempDir(), "virtiofs.sock")
	cmd := virtiofsHelperCommand(socket, "socket-and-wait")
	process, err := backend.startVirtiofsProcess("sandbox-a", cmd, socket, time.Second)
	if err != nil {
		t.Fatalf("startVirtiofsProcess() error = %v", err)
	}
	prepared := PreparedSandbox{
		Devices: []Device{{SandboxID: "sandbox-a", PID: process.handle.PID(), StartTime: process.handle.StartTime()}},
		Watch:   &ProcessWatch{process: process},
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill prepared helper: %v", err)
	}
	select {
	case <-prepared.Watch.Done():
	case <-time.After(time.Second):
		t.Fatal("prepared process exit was not published")
	}
	result, ok := prepared.Watch.Result()
	if !ok || !result.Exited || result.Signal != "SIGKILL" {
		t.Fatalf("ProcessWatch.Result() = %#v, ok=%v", result, ok)
	}
	if _, ok := backend.procs.Load("sandbox-a"); ok {
		t.Fatal("monitor did not remove exited process from backend map")
	}
	if err := backend.Cleanup("sandbox-a", prepared); err != nil {
		t.Fatalf("Cleanup() after monitor deletion error = %v", err)
	}
	if err := backend.Cleanup("sandbox-a", prepared); err != nil {
		t.Fatalf("Cleanup() repeated error = %v", err)
	}
	assertReapedBySIGKILL(t, cmd)
}

func TestActiveCleanupKillsAndReapsPreparedProcessOnce(t *testing.T) {
	backend := &virtiofsBackend{runtimeDir: t.TempDir()}
	socket := filepath.Join(t.TempDir(), "virtiofs.sock")
	cmd := virtiofsHelperCommand(socket, "socket-and-wait")
	process, err := backend.startVirtiofsProcess("sandbox-a", cmd, socket, time.Second)
	if err != nil {
		t.Fatalf("startVirtiofsProcess() error = %v", err)
	}
	prepared := PreparedSandbox{
		Devices: []Device{{SandboxID: "sandbox-a", PID: process.handle.PID(), StartTime: process.handle.StartTime()}},
		Watch:   &ProcessWatch{process: process},
	}

	if err := backend.Cleanup("sandbox-a", prepared); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	result, ok := prepared.Watch.Result()
	if !ok || !result.Exited || result.Signal != "SIGKILL" {
		t.Fatalf("cleanup observation = %#v, ok=%v", result, ok)
	}
	if err := backend.Cleanup("sandbox-a", prepared); err != nil {
		t.Fatalf("Cleanup() repeated error = %v", err)
	}
	assertReapedBySIGKILL(t, cmd)
}

func TestSocketReadyAndExitRaceNeverLosesObservation(t *testing.T) {
	for i := range 10 {
		backend := &virtiofsBackend{}
		sandboxID := "sandbox-race-" + string(rune('a'+i))
		socket := filepath.Join(t.TempDir(), "virtiofs.sock")
		cmd := virtiofsHelperCommand(socket, "socket-and-exit")
		process, err := backend.startVirtiofsProcess(sandboxID, cmd, socket, time.Second)
		if err != nil {
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 9 {
				t.Fatalf("iteration %d startup error=%v ProcessState=%#v, want reaped exit 9", i, err, cmd.ProcessState)
			}
			continue
		}
		select {
		case <-process.Done():
		case <-time.After(time.Second):
			t.Fatalf("iteration %d returned a process but lost its exit observation", i)
		}
		result, ok := process.Result()
		if !ok || !result.Exited || result.ExitCode == nil || *result.ExitCode != 9 {
			t.Fatalf("iteration %d observation=%#v ok=%v, want exit 9", i, result, ok)
		}
		if err := process.Close(); err != nil {
			t.Fatalf("iteration %d Close() error = %v", i, err)
		}
	}
}

func TestProcessRegistrationCollisionReapsNewChildAndKeepsExistingOwner(t *testing.T) {
	backend := &virtiofsBackend{}
	existing := newVirtiofsProcess("sandbox-a", newFakeProcessHandle())
	backend.procs.Store("sandbox-a", existing)
	socket := filepath.Join(t.TempDir(), "virtiofs.sock")
	cmd := virtiofsHelperCommand(socket, "never-create-socket")

	process, err := backend.startVirtiofsProcess("sandbox-a", cmd, socket, time.Second)
	if err == nil || process != nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("startVirtiofsProcess() process=%p error=%v, want collision", process, err)
	}
	assertReapedBySIGKILL(t, cmd)
	actual, ok := backend.procs.Load("sandbox-a")
	if !ok || actual != existing {
		t.Fatalf("existing owner was replaced or deleted: %#v", actual)
	}
	backend.procs.Delete("sandbox-a")
}

func assertReapedBySIGKILL(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.ProcessState == nil {
		t.Fatal("helper has no ProcessState; child was not reaped")
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("helper wait status = %#v, want SIGKILL", cmd.ProcessState.Sys())
	}
}
