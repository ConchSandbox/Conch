package volume

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAdoptedProcessRejectsIdentityChangeBeforePidfdOpen(t *testing.T) {
	openCalls := 0
	ops := adoptedProcessOps{
		readStartTime: func(int) uint64 { return 11 },
		open: func(int) (int, error) {
			openCalls++
			return 9, nil
		},
		poll:  func(int, time.Duration) (bool, error) { return false, nil },
		kill:  func(int) error { return nil },
		close: func(int) error { return nil },
	}
	if _, err := newAdoptedProcessWithOps(42, 10, ops); err == nil {
		t.Fatal("newAdoptedProcessWithOps() succeeded after pre-open identity mismatch")
	}
	if openCalls != 0 {
		t.Fatalf("pidfd open calls = %d, want 0", openCalls)
	}
}

func TestAdoptedProcessClosesPidfdOnPostOpenIdentityChange(t *testing.T) {
	reads := 0
	closedFD := -1
	ops := adoptedProcessOps{
		readStartTime: func(int) uint64 {
			reads++
			if reads == 1 {
				return 10
			}
			return 11
		},
		open:  func(int) (int, error) { return 99, nil },
		poll:  func(int, time.Duration) (bool, error) { return false, nil },
		kill:  func(int) error { return nil },
		close: func(fd int) error { closedFD = fd; return nil },
	}
	if _, err := newAdoptedProcessWithOps(42, 10, ops); err == nil {
		t.Fatal("newAdoptedProcessWithOps() succeeded after post-open identity mismatch")
	}
	if closedFD != 99 {
		t.Fatalf("closed fd = %d, want 99", closedFD)
	}
}

func TestAdoptedWaitPollErrorDoesNotClaimProcessExit(t *testing.T) {
	pollErr := errors.New("poll failed")
	process := &adoptedProcess{pid: 42, startTime: 10, pidfd: 99, ops: adoptedProcessOps{
		poll: func(int, time.Duration) (bool, error) { return false, pollErr },
	}}
	result := process.Wait()
	if result.Exited || !errors.Is(result.Cause, pollErr) {
		t.Fatalf("Wait() = %#v, want observer failure without proven exit", result)
	}
}

func TestAdoptedKillAndConfirmUseBoundPidfd(t *testing.T) {
	killedFD := -1
	pollFD := -1
	process := &adoptedProcess{pid: 42, startTime: 10, pidfd: 99, ops: adoptedProcessOps{
		kill: func(fd int) error { killedFD = fd; return nil },
		poll: func(fd int, timeout time.Duration) (bool, error) {
			pollFD = fd
			if timeout <= 0 {
				t.Fatalf("ConfirmExit timeout = %s", timeout)
			}
			return true, nil
		},
	}}
	if err := process.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if err := process.ConfirmExit(time.Second); err != nil {
		t.Fatalf("ConfirmExit() error = %v", err)
	}
	if killedFD != 99 || pollFD != 99 {
		t.Fatalf("kill fd=%d poll fd=%d, want bound pidfd 99", killedFD, pollFD)
	}
}

func TestAdoptedConfirmExitReturnsTimeout(t *testing.T) {
	process := &adoptedProcess{pid: 42, startTime: 10, pidfd: 99, ops: adoptedProcessOps{
		poll: func(int, time.Duration) (bool, error) { return false, nil },
	}}
	if err := process.ConfirmExit(10 * time.Millisecond); err == nil {
		t.Fatal("ConfirmExit() = nil, want timeout")
	}
}

func TestAdoptedProcessRealPidfdObservesAndKillsProcess(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()
	startTime := processStartTicks(cmd.Process.Pid)
	if startTime == 0 {
		t.Fatal("could not read helper process start time")
	}
	process, err := newAdoptedProcess(cmd.Process.Pid, startTime)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		t.Skipf("pidfd unavailable in test environment: %v", err)
	}
	if err != nil {
		t.Fatalf("newAdoptedProcess() error = %v", err)
	}
	defer process.Close()
	resultCh := make(chan processWaitResult, 1)
	go func() { resultCh <- process.Wait() }()
	if err := process.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	select {
	case result := <-resultCh:
		if !result.Exited || result.Cause != nil {
			t.Fatalf("Wait() = %#v, want proven pidfd exit", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pidfd Wait did not observe killed helper")
	}
	if err := process.ConfirmExit(time.Second); err != nil {
		t.Fatalf("ConfirmExit() error = %v", err)
	}
	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("cmd.Wait() error = %v, want SIGKILL ExitError", waitErr)
	}
	reaped = true
}
