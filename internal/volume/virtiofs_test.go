package volume

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	virtiofsdFixtureEnv      = "CONCH_VIRTIOFSD_PROCESS_FIXTURE"
	virtiofsdFixtureExitCode = 23
)

// TestVirtiofsdProcessFixture is re-executed as the backend daemon. It either
// exits before creating its socket, exits with a non-zero status after a
// deterministic trigger, or holds the socket until Cleanup kills it.
func TestVirtiofsdProcessFixture(t *testing.T) {
	if os.Getenv(virtiofsdFixtureEnv) != "1" {
		return
	}

	args := fixtureArguments(os.Args)
	socket := args["--socket-path"]
	mode := args["--fixture-mode"]
	trigger := args["--fixture-trigger"]
	if socket == "" || mode == "" {
		os.Exit(97)
	}
	if mode == "exit-before-socket" {
		os.Exit(virtiofsdFixtureExitCode)
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(96)
	}
	defer listener.Close()

	switch mode {
	case "exit-after-trigger":
		for {
			if _, err := os.Stat(trigger); err == nil {
				os.Exit(virtiofsdFixtureExitCode)
			}
			time.Sleep(2 * time.Millisecond)
		}
	case "hold":
		select {}
	default:
		os.Exit(95)
	}
}

func fixtureArguments(args []string) map[string]string {
	values := make(map[string]string)
	for i := 0; i+1 < len(args); i++ {
		if len(args[i]) > 2 && args[i][:2] == "--" {
			values[args[i]] = args[i+1]
			i++
		}
	}
	return values
}

func TestVirtiofsdExitBeforeSocketFailsPromptly(t *testing.T) {
	health := make(chan error, 1)
	backend, request := newVirtiofsdFixtureBackend(t, "exit-before-socket", func(err error) {
		health <- err
	})

	started := time.Now()
	devices, err := backend.Prepare(request)
	if err == nil {
		t.Fatal("Prepare() succeeded after virtiofsd exited before creating its socket")
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("Prepare() noticed pre-socket exit after %s, want less than 1s", elapsed)
	}
	if len(devices) != 0 {
		t.Fatalf("Prepare() devices = %#v, want none", devices)
	}
	assertUnhealthyExit(t, err, request, virtiofsdFixtureExitCode)
	assertProcessEntryMissing(t, backend, request.Namespace, request.SandboxID)
	if _, statErr := os.Stat(filepath.Join(backend.runtimeDir, request.SandboxID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("runtime directory stat error = %v, want not exist", statErr)
	}
	assertNoHealthSignal(t, health, "pre-socket startup failure")
}

func TestVirtiofsdExitAfterPrepareSignalsHealthAndReaps(t *testing.T) {
	health := make(chan error, 2)
	backend, request := newVirtiofsdFixtureBackend(t, "exit-after-trigger", func(err error) {
		health <- err
	})

	devices, err := backend.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	process := loadManagedVirtiofsd(t, backend, request.Namespace, request.SandboxID)

	if err := os.WriteFile(fixtureTriggerPath(backend, request.SandboxID), []byte("exit"), 0o600); err != nil {
		t.Fatalf("write fixture trigger: %v", err)
	}
	select {
	case healthErr := <-health:
		assertUnhealthyExit(t, healthErr, request, virtiofsdFixtureExitCode)
	case <-time.After(time.Second):
		t.Fatal("unexpected virtiofsd exit did not produce a health signal")
	}

	waitForManagedVirtiofsdDone(t, process)
	assertProcessEntryMissing(t, backend, request.Namespace, request.SandboxID)
	if process.cmd.ProcessState == nil {
		t.Fatal("unexpectedly exited virtiofsd was not reaped")
	}
	assertNoHealthSignal(t, health, "duplicate unexpected-exit notification")

	started := time.Now()
	if err := backend.Cleanup(request.Namespace, request.SandboxID, devices); err != nil {
		t.Fatalf("Cleanup() after unexpected exit error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("Cleanup() after unexpected exit took %s, want less than 250ms", elapsed)
	}
	manager := &Manager{backend: backend}
	assertUnhealthyExit(t, manager.CheckSandboxHealth(request.Namespace, request.SandboxID), request, virtiofsdFixtureExitCode)
	manager.ClearSandboxHealth(request.Namespace, request.SandboxID)
	if err := manager.CheckSandboxHealth(request.Namespace, request.SandboxID); err != nil {
		t.Fatalf("CheckSandboxHealth() after clear = %v, want nil", err)
	}
}

func TestVirtiofsdNormalCleanupIsExpectedAndIdempotent(t *testing.T) {
	health := make(chan error, 1)
	backend, request := newVirtiofsdFixtureBackend(t, "hold", func(err error) {
		health <- err
	})
	devices, err := backend.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	process := loadManagedVirtiofsd(t, backend, request.Namespace, request.SandboxID)

	if err := backend.Cleanup(request.Namespace, request.SandboxID, devices); err != nil {
		t.Fatalf("first Cleanup() error = %v", err)
	}
	waitForManagedVirtiofsdDone(t, process)
	assertProcessEntryMissing(t, backend, request.Namespace, request.SandboxID)
	if process.cmd.ProcessState == nil {
		t.Fatal("normally cleaned virtiofsd was not reaped")
	}
	assertNoHealthSignal(t, health, "expected cleanup")

	started := time.Now()
	if err := backend.Cleanup(request.Namespace, request.SandboxID, devices); err != nil {
		t.Fatalf("second Cleanup() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("idempotent Cleanup() took %s, want less than 250ms", elapsed)
	}
}

func TestVirtiofsdConcurrentCleanupKillsAndWaitsOnce(t *testing.T) {
	health := make(chan error, 1)
	backend, request := newVirtiofsdFixtureBackend(t, "hold", func(err error) {
		health <- err
	})
	var killCalls atomic.Int32
	backend.ops.kill = func(process *os.Process) error {
		killCalls.Add(1)
		return process.Kill()
	}
	devices, err := backend.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	process := loadManagedVirtiofsd(t, backend, request.Namespace, request.SandboxID)

	const callers = 16
	start := make(chan struct{})
	results := make(chan error, callers)
	var callersReady sync.WaitGroup
	callersReady.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			callersReady.Done()
			<-start
			results <- backend.Cleanup(request.Namespace, request.SandboxID, devices)
		}()
	}
	callersReady.Wait()
	close(start)
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Cleanup() error = %v", err)
		}
	}

	waitForManagedVirtiofsdDone(t, process)
	assertProcessEntryMissing(t, backend, request.Namespace, request.SandboxID)
	if got := killCalls.Load(); got != 1 {
		t.Fatalf("process kill calls = %d, want 1", got)
	}
	assertNoHealthSignal(t, health, "concurrent expected cleanup")
}

func TestVirtiofsdCleanupRetainsHealthPublishedAtStopBoundary(t *testing.T) {
	backend, request := newVirtiofsdFixtureBackend(t, "hold", nil)
	devices, err := backend.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	key := newSandboxProcessKey(request.Namespace, request.SandboxID)
	healthErr := &UnhealthyError{
		Namespace: request.Namespace,
		SandboxID: request.SandboxID,
		PID:       devices[0].PID,
		Cause:     errVirtiofsdExited,
	}
	backend.ops.kill = func(process *os.Process) error {
		// Model the monitor winning the stopping mutex after Cleanup's first
		// health lookup but publishing before its done channel closes.
		backend.health.Store(key, healthErr)
		return process.Kill()
	}

	if err := backend.Cleanup(request.Namespace, request.SandboxID, devices); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if got := backend.CheckHealth(request.Namespace, request.SandboxID); got != healthErr {
		t.Fatalf("retained health = %v, want %v", got, healthErr)
	}
	backend.ClearHealth(request.Namespace, request.SandboxID)
}

func TestVirtiofsdCleanupPropagatesKillErrorPromptly(t *testing.T) {
	errKill := errors.New("injected kill failure")
	health := make(chan error, 1)
	backend, request := newVirtiofsdFixtureBackend(t, "hold", func(err error) {
		health <- err
	})
	var killCalls atomic.Int32
	backend.ops.kill = func(*os.Process) error {
		killCalls.Add(1)
		return errKill
	}
	devices, err := backend.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	process := loadManagedVirtiofsd(t, backend, request.Namespace, request.SandboxID)

	started := time.Now()
	err = backend.Cleanup(request.Namespace, request.SandboxID, devices)
	if !errors.Is(err, errKill) {
		t.Fatalf("Cleanup() error = %v, want injected kill failure", err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("Cleanup() with kill failure took %s, want less than 250ms", elapsed)
	}
	if got := killCalls.Load(); got != 1 {
		t.Fatalf("process kill calls after failed cleanup = %d, want 1", got)
	}
	assertProcessEntryMissing(t, backend, request.Namespace, request.SandboxID)
	waitForManagedVirtiofsdDone(t, process)

	// Cleanup reports the owner error but converges through the persisted
	// identity fallback without creating a second Cmd.Wait owner.
	if err := backend.Cleanup(request.Namespace, request.SandboxID, devices); err != nil {
		t.Fatalf("converged Cleanup() error = %v", err)
	}
	assertProcessEntryMissing(t, backend, request.Namespace, request.SandboxID)
	assertNoHealthSignal(t, health, "failed expected cleanup")
	if err := backend.Cleanup(request.Namespace, request.SandboxID, devices); err != nil {
		t.Fatalf("converged Cleanup() error = %v", err)
	}
}

func TestRestoredVirtiofsdUnexpectedExitSignalsHealth(t *testing.T) {
	fixture := newIssue083RestoredFixture(t)
	backend := fixture.backend.(*virtiofsBackend)
	manager := &Manager{backend: backend}
	health := make(chan error, 2)
	if err := manager.RestoreSandboxWithHealth(fixture.namespace, fixture.sandboxID, []Device{fixture.device}, func(err error) {
		health <- err
	}); err != nil {
		t.Fatalf("RestoreSandboxWithHealth() error = %v", err)
	}
	process := loadRestoredVirtiofsd(t, backend, fixture.namespace, fixture.sandboxID)

	fixture.exit(t)
	var healthErr error
	select {
	case healthErr = <-health:
	case <-time.After(time.Second):
		t.Fatal("restored virtiofsd death did not produce a health signal")
	}
	assertRestoredUnhealthy(t, healthErr, fixture)
	waitForManagedVirtiofsdDone(t, process)
	assertProcessEntryMissing(t, backend, fixture.namespace, fixture.sandboxID)
	if process.cmd != nil {
		t.Fatal("restored process unexpectedly owns an exec.Cmd")
	}
	if fixture.cmd.ProcessState != nil {
		t.Fatal("restored-process watcher called Wait on a non-child")
	}
	assertRestoredUnhealthy(t, manager.CheckSandboxHealth(fixture.namespace, fixture.sandboxID), fixture)
	assertNoHealthSignal(t, health, "duplicate restored-process exit")

	if err := manager.CleanupSandbox(fixture.namespace, fixture.sandboxID, []Device{fixture.device}); err != nil {
		t.Fatalf("CleanupSandbox() after restored exit error = %v", err)
	}
	assertRestoredUnhealthy(t, manager.CheckSandboxHealth(fixture.namespace, fixture.sandboxID), fixture)
	manager.ClearSandboxHealth(fixture.namespace, fixture.sandboxID)
	if err := manager.CheckSandboxHealth(fixture.namespace, fixture.sandboxID); err != nil {
		t.Fatalf("CheckSandboxHealth() after explicit clear = %v, want nil", err)
	}
	if err := fixture.reap(); err != nil {
		t.Fatalf("fixture Wait() error = %v", err)
	}
}

func TestRestoredVirtiofsdExpectedCleanupSuppressesHealth(t *testing.T) {
	fixture := newIssue083RestoredFixture(t)
	backend := fixture.backend.(*virtiofsBackend)
	health := make(chan error, 1)
	if err := backend.RestoreWithHealth(fixture.namespace, fixture.sandboxID, []Device{fixture.device}, func(err error) {
		health <- err
	}); err != nil {
		t.Fatalf("RestoreWithHealth() error = %v", err)
	}
	process := loadRestoredVirtiofsd(t, backend, fixture.namespace, fixture.sandboxID)

	if err := backend.Cleanup(fixture.namespace, fixture.sandboxID, []Device{fixture.device}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	waitForManagedVirtiofsdDone(t, process)
	assertProcessEntryMissing(t, backend, fixture.namespace, fixture.sandboxID)
	assertNoHealthSignal(t, health, "expected restored-process cleanup")
	if err := backend.CheckHealth(fixture.namespace, fixture.sandboxID); err != nil {
		t.Fatalf("CheckHealth() after expected Cleanup = %v, want nil", err)
	}
	if fixture.cmd.ProcessState != nil {
		t.Fatal("restored-process cleanup called Wait on a non-child")
	}
	_ = fixture.reap()
}

func TestRestoredVirtiofsdCleanupStopsAndJoinsWatcher(t *testing.T) {
	fixture := newIssue083RestoredFixture(t)
	backend := fixture.backend.(*virtiofsBackend)
	realPoll := backend.ops.poll
	realClose := backend.ops.closeFD
	var pollCalls atomic.Int32
	var closeCalls atomic.Int32
	backend.ops.poll = func(fds []unix.PollFd, timeout int) (int, error) {
		pollCalls.Add(1)
		return realPoll(fds, timeout)
	}
	backend.ops.closeFD = func(fd int) error {
		closeCalls.Add(1)
		return realClose(fd)
	}
	if err := backend.RestoreWithHealth(fixture.namespace, fixture.sandboxID, []Device{fixture.device}, nil); err != nil {
		t.Fatalf("RestoreWithHealth() error = %v", err)
	}
	process := loadRestoredVirtiofsd(t, backend, fixture.namespace, fixture.sandboxID)
	waitForAtomicAtLeast(t, &pollCalls, 1, "restored watcher poll")

	if err := backend.Cleanup(fixture.namespace, fixture.sandboxID, []Device{fixture.device}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	select {
	case <-process.watchStop:
	default:
		t.Fatal("Cleanup() did not cancel the restored watcher")
	}
	waitForManagedVirtiofsdDone(t, process)
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("pidfd close calls = %d, want 1", got)
	}
	completedPolls := pollCalls.Load()
	time.Sleep(3 * processPollInterval)
	if got := pollCalls.Load(); got != completedPolls {
		t.Fatalf("watcher kept polling after Cleanup: calls %d -> %d", completedPolls, got)
	}
	assertProcessEntryMissing(t, backend, fixture.namespace, fixture.sandboxID)
	_ = fixture.reap()
}

func TestRestoredVirtiofsdCleanupPropagatesSignalErrorAndRetries(t *testing.T) {
	fixture := newIssue083RestoredFixture(t)
	backend := fixture.backend.(*virtiofsBackend)
	errSignal := errors.New("injected pidfd signal failure")
	realSendSignal := backend.ops.pidfdSendSignal
	var signalCalls atomic.Int32
	backend.ops.pidfdSendSignal = func(pidfd int, signal unix.Signal, info *unix.Siginfo, flags int) error {
		if signalCalls.Add(1) == 1 {
			return errSignal
		}
		return realSendSignal(pidfd, signal, info, flags)
	}
	if err := backend.RestoreWithHealth(fixture.namespace, fixture.sandboxID, []Device{fixture.device}, nil); err != nil {
		t.Fatalf("RestoreWithHealth() error = %v", err)
	}
	process := loadRestoredVirtiofsd(t, backend, fixture.namespace, fixture.sandboxID)

	started := time.Now()
	err := backend.Cleanup(fixture.namespace, fixture.sandboxID, []Device{fixture.device})
	if !errors.Is(err, errSignal) {
		t.Fatalf("Cleanup() error = %v, want injected signal failure", err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("Cleanup() with signal failure took %s, want less than 250ms", elapsed)
	}
	waitForManagedVirtiofsdDone(t, process)
	assertProcessEntryMissing(t, backend, fixture.namespace, fixture.sandboxID)
	if got := signalCalls.Load(); got != 2 {
		t.Fatalf("pidfd signal calls = %d, want failed owner attempt plus safe fallback", got)
	}
	_ = fixture.reap()
	if err := backend.Cleanup(fixture.namespace, fixture.sandboxID, []Device{fixture.device}); err != nil {
		t.Fatalf("converged Cleanup() error = %v", err)
	}
	if got := signalCalls.Load(); got != 2 {
		t.Fatalf("pidfd signal calls after converged Cleanup = %d, want 2", got)
	}
}

func TestRestoredVirtiofsdWatcherErrorPropagatesAndCleanupConverges(t *testing.T) {
	fixture := newIssue083RestoredFixture(t)
	backend := fixture.backend.(*virtiofsBackend)
	errPoll := errors.New("injected pidfd poll failure")
	realPoll := backend.ops.poll
	backend.ops.poll = func([]unix.PollFd, int) (int, error) {
		return 0, errPoll
	}
	health := make(chan error, 1)
	if err := backend.RestoreWithHealth(fixture.namespace, fixture.sandboxID, []Device{fixture.device}, func(err error) {
		health <- err
	}); err != nil {
		t.Fatalf("RestoreWithHealth() error = %v", err)
	}
	process := loadRestoredVirtiofsd(t, backend, fixture.namespace, fixture.sandboxID)
	select {
	case healthErr := <-health:
		if !errors.Is(healthErr, ErrBackendUnhealthy) || !errors.Is(healthErr, errPoll) {
			t.Fatalf("watcher health error = %v, want injected poll failure", healthErr)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher poll failure did not produce a health signal")
	}
	waitForManagedVirtiofsdDone(t, process)
	if got := loadRestoredVirtiofsd(t, backend, fixture.namespace, fixture.sandboxID); got != process {
		t.Fatalf("retained process after watcher failure = %p, want %p", got, process)
	}

	// The failed watcher keeps its pidfd ownership so Cleanup can still kill
	// the original process safely, then drop the map entry and descriptor.
	backend.ops.poll = realPoll
	if err := backend.Cleanup(fixture.namespace, fixture.sandboxID, []Device{fixture.device}); err != nil {
		t.Fatalf("Cleanup() after watcher failure error = %v", err)
	}
	assertProcessEntryMissing(t, backend, fixture.namespace, fixture.sandboxID)
	if err := backend.CheckHealth(fixture.namespace, fixture.sandboxID); !errors.Is(err, errPoll) {
		t.Fatalf("retained watcher health = %v, want injected poll failure", err)
	}
	backend.ClearHealth(fixture.namespace, fixture.sandboxID)
	_ = fixture.reap()
}

func TestRestoredVirtiofsdPIDReuseNeverSignalsNewProcess(t *testing.T) {
	fixture := newIssue083RestoredFixture(t)
	backend := fixture.backend.(*virtiofsBackend)
	recordedStart := fixture.device.StartTime
	const fakePIDFD = 4242
	var identityReads atomic.Int32
	var openCalls atomic.Int32
	var closeCalls atomic.Int32
	var signalCalls atomic.Int32
	backend.ops.processStart = func(pid int) uint64 {
		if pid != fixture.device.PID {
			t.Fatalf("processStart pid = %d, want %d", pid, fixture.device.PID)
		}
		if identityReads.Add(1) == 1 {
			return recordedStart
		}
		return recordedStart + 1
	}
	backend.ops.pidfdOpen = func(pid, flags int) (int, error) {
		openCalls.Add(1)
		return fakePIDFD, nil
	}
	backend.ops.closeFD = func(fd int) error {
		if fd != fakePIDFD {
			t.Fatalf("close fd = %d, want %d", fd, fakePIDFD)
		}
		closeCalls.Add(1)
		return nil
	}
	backend.ops.pidfdSendSignal = func(int, unix.Signal, *unix.Siginfo, int) error {
		signalCalls.Add(1)
		return nil
	}

	err := backend.RestoreWithHealth(fixture.namespace, fixture.sandboxID, []Device{fixture.device}, nil)
	if !errors.Is(err, errProcessIdentityChanged) {
		t.Fatalf("RestoreWithHealth() error = %v, want process identity change", err)
	}
	if got := openCalls.Load(); got != 1 {
		t.Fatalf("pidfd open calls = %d, want 1", got)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("pidfd close calls = %d, want 1", got)
	}
	if got := signalCalls.Load(); got != 0 {
		t.Fatalf("pidfd signal calls after identity reuse = %d, want 0", got)
	}
	assertProcessEntryMissing(t, backend, fixture.namespace, fixture.sandboxID)

	// Cleanup sees the changed start time before opening another pidfd. It
	// converges without ever addressing the process that reused the PID.
	if err := backend.Cleanup(fixture.namespace, fixture.sandboxID, []Device{fixture.device}); err != nil {
		t.Fatalf("Cleanup() after identity reuse error = %v", err)
	}
	if got := openCalls.Load(); got != 1 {
		t.Fatalf("pidfd open calls after Cleanup = %d, want 1", got)
	}
	if got := signalCalls.Load(); got != 0 {
		t.Fatalf("pidfd signal calls after Cleanup = %d, want 0", got)
	}
}

func TestVirtiofsdProcessTrackingIsIsolatedByNamespace(t *testing.T) {
	first := newIssue083RestoredFixture(t)
	second := newIssue083RestoredFixture(t)
	backend := first.backend.(*virtiofsBackend)
	firstNamespace := "issue-083-ns-a"
	secondNamespace := "issue-083-ns-b"
	firstDevice := first.device
	firstDevice.Namespace = firstNamespace
	secondDevice := second.device
	secondDevice.Namespace = secondNamespace

	if err := backend.RestoreWithHealth(firstNamespace, first.sandboxID, []Device{firstDevice}, nil); err != nil {
		t.Fatalf("restore first namespace: %v", err)
	}
	if err := backend.RestoreWithHealth(secondNamespace, second.sandboxID, []Device{secondDevice}, nil); err != nil {
		t.Fatalf("restore second namespace: %v", err)
	}
	firstProcess := loadRestoredVirtiofsd(t, backend, firstNamespace, first.sandboxID)
	secondProcess := loadRestoredVirtiofsd(t, backend, secondNamespace, second.sandboxID)

	if err := backend.Cleanup(firstNamespace, first.sandboxID, []Device{firstDevice}); err != nil {
		t.Fatalf("Cleanup() first namespace error = %v", err)
	}
	waitForManagedVirtiofsdDone(t, firstProcess)
	assertProcessEntryMissing(t, backend, firstNamespace, first.sandboxID)
	if got := loadRestoredVirtiofsd(t, backend, secondNamespace, second.sandboxID); got != secondProcess {
		t.Fatalf("second namespace process = %p, want %p", got, secondProcess)
	}
	if processStartTicks(second.device.PID) != second.device.StartTime {
		t.Fatal("cleanup in first namespace terminated the second namespace process")
	}

	if err := backend.Cleanup(secondNamespace, second.sandboxID, []Device{secondDevice}); err != nil {
		t.Fatalf("Cleanup() second namespace error = %v", err)
	}
	waitForManagedVirtiofsdDone(t, secondProcess)
	_ = first.reap()
	_ = second.reap()
}

func newVirtiofsdFixtureBackend(t *testing.T, mode string, onUnhealthy func(error)) (*virtiofsBackend, PrepareRequest) {
	t.Helper()
	runtimeDir, err := os.MkdirTemp("", "conch-vfs-")
	if err != nil {
		t.Fatalf("create short runtime directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	sourceDir := t.TempDir()
	sandboxID := "sandbox-" + mode
	trigger := fixtureTriggerPathForRuntime(runtimeDir, sandboxID)

	backend := NewVirtiofsBackend(VirtiofsConfig{
		Binary:     os.Args[0],
		RuntimeDir: runtimeDir,
	}).(*virtiofsBackend)
	backend.ops.mount = func(string, string, string, uintptr, string) error { return nil }
	backend.ops.unmount = func(string, int) error { return nil }
	backend.ops.command = func(_ string, args ...string) *exec.Cmd {
		fixtureArgs := []string{
			"-test.run=^TestVirtiofsdProcessFixture$",
			"--",
		}
		fixtureArgs = append(fixtureArgs, args...)
		fixtureArgs = append(fixtureArgs,
			"--fixture-mode", mode,
			"--fixture-trigger", trigger,
		)
		cmd := exec.Command(os.Args[0], fixtureArgs...)
		cmd.Env = append(os.Environ(), virtiofsdFixtureEnv+"=1")
		return cmd
	}

	t.Cleanup(func() {
		backend.procs.Range(func(_, value any) bool {
			if process, ok := value.(*virtiofsProcess); ok && process.cmd != nil && process.cmd.Process != nil {
				_ = process.cmd.Process.Kill()
				select {
				case <-process.done:
				case <-time.After(time.Second):
				}
			}
			return true
		})
	})

	return backend, PrepareRequest{
		Namespace: "test-ns",
		SandboxID: sandboxID,
		Mounts: []Mount{{
			Source: sourceDir,
			Path:   "/data",
		}},
		OnUnhealthy: onUnhealthy,
	}
}

func fixtureTriggerPath(backend *virtiofsBackend, sandboxID string) string {
	return fixtureTriggerPathForRuntime(backend.runtimeDir, sandboxID)
}

func fixtureTriggerPathForRuntime(runtimeDir, sandboxID string) string {
	return filepath.Join(runtimeDir, sandboxID+".exit")
}

func loadManagedVirtiofsd(t *testing.T, backend *virtiofsBackend, namespace, sandboxID string) *virtiofsProcess {
	t.Helper()
	process := loadVirtiofsProcess(t, backend, namespace, sandboxID)
	if process.cmd == nil || process.cmd.Process == nil || process.adopted {
		t.Fatal("managed child does not contain a started exec.Cmd")
	}
	return process
}

func loadVirtiofsProcess(t *testing.T, backend *virtiofsBackend, namespace, sandboxID string) *virtiofsProcess {
	t.Helper()
	value, ok := backend.procs.Load(newSandboxProcessKey(namespace, sandboxID))
	if !ok {
		t.Fatalf("process map has no entry for %s", sandboxID)
	}
	process, ok := value.(*virtiofsProcess)
	if !ok {
		t.Fatalf("process map entry type = %T, want *virtiofsProcess", value)
	}
	return process
}

func loadRestoredVirtiofsd(t *testing.T, backend *virtiofsBackend, namespace, sandboxID string) *virtiofsProcess {
	t.Helper()
	process := loadVirtiofsProcess(t, backend, namespace, sandboxID)
	if !process.adopted || process.cmd != nil || process.pidfd < 0 || process.watchStop == nil {
		t.Fatalf("restored process state = %#v, want adopted pidfd watcher", process)
	}
	return process
}

func waitForManagedVirtiofsdDone(t *testing.T, process *virtiofsProcess) {
	t.Helper()
	select {
	case <-process.done:
	case <-time.After(time.Second):
		t.Fatal("virtiofsd monitor did not finish within 1s")
	}
}

func waitForAtomicAtLeast(t *testing.T, value *atomic.Int32, want int32, context string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if value.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s count = %d, want at least %d", context, value.Load(), want)
}

func assertProcessEntryMissing(t *testing.T, backend *virtiofsBackend, namespace, sandboxID string) {
	t.Helper()
	if value, ok := backend.procs.Load(newSandboxProcessKey(namespace, sandboxID)); ok {
		t.Fatalf("stale process map entry remains for %s: %T", sandboxID, value)
	}
}

func assertNoHealthSignal(t *testing.T, health <-chan error, context string) {
	t.Helper()
	select {
	case err := <-health:
		t.Fatalf("health signal for %s = %v, want none", context, err)
	case <-time.After(50 * time.Millisecond):
	}
}

func assertUnhealthyExit(t *testing.T, err error, request PrepareRequest, exitCode int) {
	t.Helper()
	if !errors.Is(err, ErrBackendUnhealthy) {
		t.Fatalf("error = %v, want ErrBackendUnhealthy", err)
	}
	var unhealthy *UnhealthyError
	if !errors.As(err, &unhealthy) {
		t.Fatalf("error type = %T, want *UnhealthyError", err)
	}
	if unhealthy.Namespace != request.Namespace || unhealthy.SandboxID != request.SandboxID || unhealthy.PID <= 0 {
		t.Fatalf("unhealthy error metadata = %#v", unhealthy)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %v, want wrapped *exec.ExitError", err)
	}
	if got := exitErr.ExitCode(); got != exitCode {
		t.Fatalf("exit code = %d, want %d", got, exitCode)
	}
}

func assertRestoredUnhealthy(t *testing.T, err error, fixture *issue083RestoredFixture) {
	t.Helper()
	if !errors.Is(err, ErrBackendUnhealthy) || !errors.Is(err, errVirtiofsdExited) {
		t.Fatalf("error = %v, want restored ErrBackendUnhealthy exit", err)
	}
	var unhealthy *UnhealthyError
	if !errors.As(err, &unhealthy) {
		t.Fatalf("error type = %T, want *UnhealthyError", err)
	}
	if unhealthy.Namespace != fixture.namespace || unhealthy.SandboxID != fixture.sandboxID || unhealthy.PID != fixture.device.PID {
		t.Fatalf("unhealthy error metadata = %#v", unhealthy)
	}
}
