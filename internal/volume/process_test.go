package volume

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fakeProcessHandle struct {
	pid        int
	startTime  uint64
	waitCh     chan processWaitResult
	killWait   *processWaitResult
	confirmErr error
	closeErr   error

	mu           sync.Mutex
	waitCalls    int
	killCalls    int
	confirmCalls int
	closeCalls   int
	killOnce     sync.Once
}

func newFakeProcessHandle() *fakeProcessHandle {
	return &fakeProcessHandle{pid: 42, startTime: 84, waitCh: make(chan processWaitResult, 1)}
}

func (h *fakeProcessHandle) PID() int { return h.pid }

func (h *fakeProcessHandle) StartTime() uint64 { return h.startTime }

func (h *fakeProcessHandle) Wait() processWaitResult {
	h.mu.Lock()
	h.waitCalls++
	h.mu.Unlock()
	return <-h.waitCh
}

func (h *fakeProcessHandle) Kill() error {
	h.mu.Lock()
	h.killCalls++
	h.mu.Unlock()
	if h.killWait != nil {
		h.killOnce.Do(func() { h.waitCh <- *h.killWait })
	}
	return nil
}

func (h *fakeProcessHandle) ConfirmExit(time.Duration) error {
	h.mu.Lock()
	h.confirmCalls++
	h.mu.Unlock()
	return h.confirmErr
}

func (h *fakeProcessHandle) Close() error {
	h.mu.Lock()
	h.closeCalls++
	h.mu.Unlock()
	return h.closeErr
}

func (h *fakeProcessHandle) counts() (wait, kill, confirm, close int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waitCalls, h.killCalls, h.confirmCalls, h.closeCalls
}

func TestProcessWatchBroadcastsOneImmutableObservation(t *testing.T) {
	backend := &virtiofsBackend{}
	handle := newFakeProcessHandle()
	process := newVirtiofsProcess("sandbox-a", handle)
	watch := &ProcessWatch{process: process}
	backend.procs.Store("sandbox-a", process)
	go backend.monitorProcess(process)

	results := make(chan ProcessObservation, 2)
	for range 2 {
		go func() {
			<-watch.Done()
			result, ok := watch.Result()
			if !ok {
				results <- ProcessObservation{PID: -1}
				return
			}
			results <- result
		}()
	}
	exitCode := 7
	handle.waitCh <- processWaitResult{Exited: true, ExitCode: &exitCode}

	for range 2 {
		select {
		case result := <-results:
			if result.PID != 42 || !result.Exited || result.ExitCode == nil || *result.ExitCode != 7 {
				t.Fatalf("observation = %#v", result)
			}
			*result.ExitCode = 99
		case <-time.After(time.Second):
			t.Fatal("watcher did not receive monitor completion")
		}
	}
	stored, ok := watch.Result()
	if !ok || stored.ExitCode == nil || *stored.ExitCode != 7 {
		t.Fatalf("stored observation was mutable through caller result: %#v", stored)
	}
	if _, ok := backend.procs.Load("sandbox-a"); ok {
		t.Fatal("completed process remains in backend map")
	}
}

func TestVirtiofsProcessCloseIsStrictOnceAndWaitsOnlyInMonitor(t *testing.T) {
	backend := &virtiofsBackend{}
	handle := newFakeProcessHandle()
	exit := processWaitResult{Exited: true, Signal: "SIGKILL", Cause: errors.New("signal: killed")}
	handle.killWait = &exit
	process := newVirtiofsProcess("sandbox-a", handle)
	backend.procs.Store("sandbox-a", process)
	go backend.monitorProcess(process)

	const callers = 8
	errs := make(chan error, callers)
	for range callers {
		go func() { errs <- process.Close() }()
	}
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
	wait, kill, confirm, close := handle.counts()
	if wait != 1 || kill != 1 || confirm != 0 || close != 1 {
		t.Fatalf("calls wait=%d kill=%d confirm=%d close=%d, want 1,1,0,1", wait, kill, confirm, close)
	}
}

func TestVirtiofsProcessCloseConfirmsExitAfterObserverFailure(t *testing.T) {
	backend := &virtiofsBackend{}
	handle := newFakeProcessHandle()
	handle.waitCh <- processWaitResult{Exited: false, Cause: errors.New("poll failed")}
	process := newVirtiofsProcess("sandbox-a", handle)
	go backend.monitorProcess(process)
	<-process.Done()

	if err := process.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	wait, kill, confirm, close := handle.counts()
	if wait != 1 || kill != 1 || confirm != 1 || close != 1 {
		t.Fatalf("calls wait=%d kill=%d confirm=%d close=%d, want 1,1,1,1", wait, kill, confirm, close)
	}
}

func TestVirtiofsProcessClosePreservesConfirmationFailure(t *testing.T) {
	backend := &virtiofsBackend{}
	confirmErr := errors.New("pidfd never became ready")
	handle := newFakeProcessHandle()
	handle.waitCh <- processWaitResult{Exited: false, Cause: errors.New("poll failed")}
	handle.confirmErr = confirmErr
	process := newVirtiofsProcess("sandbox-a", handle)
	go backend.monitorProcess(process)
	<-process.Done()

	if err := process.Close(); !errors.Is(err, confirmErr) {
		t.Fatalf("Close() error = %v, want confirmation failure", err)
	}
}

func TestMarkPreparedRejectsObservationAlreadyPublished(t *testing.T) {
	backend := &virtiofsBackend{}
	handle := newFakeProcessHandle()
	handle.waitCh <- processWaitResult{Exited: true, Cause: errors.New("exit status 3")}
	process := newVirtiofsProcess("sandbox-a", handle)
	go backend.monitorProcess(process)
	<-process.Done()

	if err := process.markPrepared(); err == nil {
		t.Fatal("markPrepared() = nil after monitor published exit")
	}
}

func TestVirtiofsMonitorDoesNotDeleteReplacementProcess(t *testing.T) {
	backend := &virtiofsBackend{}
	oldHandle := newFakeProcessHandle()
	oldProcess := newVirtiofsProcess("sandbox-a", oldHandle)
	backend.procs.Store("sandbox-a", oldProcess)
	go backend.monitorProcess(oldProcess)

	replacement := newVirtiofsProcess("sandbox-a", newFakeProcessHandle())
	backend.procs.Store("sandbox-a", replacement)
	oldHandle.waitCh <- processWaitResult{Exited: true}
	<-oldProcess.Done()

	actual, ok := backend.procs.Load("sandbox-a")
	if !ok || actual != replacement {
		t.Fatalf("old monitor removed replacement process: %#v", actual)
	}
	backend.procs.Delete("sandbox-a")
}

func TestChildProcessSuccessfulWaitProvesExit(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	child, err := newChildProcess(cmd, 123)
	if err != nil {
		t.Fatal(err)
	}
	result := child.Wait()
	if !result.Exited || result.Cause != nil || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("Wait() = %#v, want proven exit code 0", result)
	}
}

func TestChildProcessExitErrorStillProvesExitAndIsCached(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	child, err := newChildProcess(cmd, 123)
	if err != nil {
		t.Fatal(err)
	}
	first := child.Wait()
	second := child.Wait()
	if !first.Exited || first.ExitCode == nil || *first.ExitCode != 7 || first.Cause == nil {
		t.Fatalf("first Wait() = %#v", first)
	}
	if !second.Exited || second.ExitCode == nil || *second.ExitCode != 7 {
		t.Fatalf("cached Wait() = %#v", second)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("helper child was not reaped")
	}
}

func TestChildProcessReportsSignal(t *testing.T) {
	cmd := exec.Command("sh", "-c", "kill -9 $$")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	child, err := newChildProcess(cmd, 123)
	if err != nil {
		t.Fatal(err)
	}
	result := child.Wait()
	if !result.Exited || result.Signal != "SIGKILL" || result.Cause == nil {
		t.Fatalf("Wait() = %#v, want proven SIGKILL exit", result)
	}
}

func TestChildProcessCleanupKillsAndReapsRealProcess(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()
	child, err := newChildProcess(cmd, 123)
	if err != nil {
		t.Fatal(err)
	}
	backend := &virtiofsBackend{}
	process := newVirtiofsProcess("sandbox-a", child)
	backend.procs.Store("sandbox-a", process)
	go backend.monitorProcess(process)

	if err := process.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	cleaned = true
	if cmd.ProcessState == nil {
		t.Fatal("helper child has no ProcessState; monitor did not reap it")
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("helper wait status = %#v, want SIGKILL", cmd.ProcessState.Sys())
	}
	result, ok := process.Result()
	if !ok || !result.Exited {
		t.Fatalf("observation = %#v, ok=%v", result, ok)
	}
}
