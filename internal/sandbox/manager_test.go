package sandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/volume"
)

func TestDurationOrDefault(t *testing.T) {
	const fallback = 10 * time.Millisecond
	if got := durationOrDefault(0, fallback); got != fallback {
		t.Fatalf("durationOrDefault(0) = %s, want %s", got, fallback)
	}
	if got := durationOrDefault(time.Second, fallback); got != time.Second {
		t.Fatalf("durationOrDefault(1s) = %s, want 1s", got)
	}
}

func TestReserveSandboxEntryDoesNotBlockDifferentSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	defer m.sandboxes.CompareAndDelete(key, entry)

	done := make(chan error, 1)
	go func() {
		otherKey, otherEntry, err := m.reserveSandboxEntry("sandbox-b")
		if err == nil {
			m.sandboxes.CompareAndDelete(otherKey, otherEntry)
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reserveSandboxEntry() for different sandbox error = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("reserveSandboxEntry() for different sandbox blocked behind another sandbox entry")
	}
}

func TestCreatingVolumeExitRecordsCauseBeforeCancel(t *testing.T) {
	m := &Manager{}
	entry := &sandboxEntry{state: sandboxCreating, cleanupDone: make(chan struct{})}
	m.sandboxes.Store("sandbox-a", entry)
	waitErr := errors.New("wait reported exit 7")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	exitCode := 7

	m.handleVolumeProcessObservation("sandbox-a", entry, volume.ProcessObservation{
		PID:      42,
		Exited:   true,
		Cause:    waitErr,
		ExitCode: &exitCode,
	}, cancel)

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("create context was not canceled after virtiofsd exit")
	}
	if !errors.Is(context.Cause(ctx), waitErr) {
		t.Fatalf("context cause = %v, want wrapped wait error", context.Cause(ctx))
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.dependencyErr == nil || !errors.Is(entry.dependencyErr, waitErr) {
		t.Fatalf("dependencyErr = %v, want wrapped wait error", entry.dependencyErr)
	}
	if entry.state != sandboxCreating {
		t.Fatalf("state = %s, want creating until Create owner rolls back", entry.state)
	}
}

func TestCommitReadyRejectsRecordedDependencyFailure(t *testing.T) {
	m := &Manager{}
	dependencyErr := errors.New("virtiofsd exited during create")
	entry := &sandboxEntry{
		state:         sandboxCreating,
		dependencyErr: dependencyErr,
		cleanupDone:   make(chan struct{}),
	}
	m.sandboxes.Store("sandbox-a", entry)

	err := m.commitReady("sandbox-a", entry, &Sandbox{sandboxID: "sandbox-a"})
	if !errors.Is(err, dependencyErr) {
		t.Fatalf("commitReady() error = %v, want dependency failure", err)
	}
	if entry.state != sandboxCreating || entry.sbx != nil {
		t.Fatalf("entry published despite dependency failure: state=%s sbx=%p", entry.state, entry.sbx)
	}
}

func TestReadyVolumeObservationWithoutCauseStillCleansSandbox(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot, cidAllocator: NewCIDAllocator()}
	var cleanupCalls atomic.Int32
	sbx := &Sandbox{cleanup: NewCleanup(), sandboxID: "sandbox-a"}
	sbx.cleanup.Add(func(context.Context) error {
		cleanupCalls.Add(1)
		return nil
	})
	entry := &sandboxEntry{state: sandboxReady, sbx: sbx, cleanupDone: make(chan struct{})}
	m.sandboxes.Store("sandbox-a", entry)

	m.handleVolumeProcessObservation("sandbox-a", entry, volume.ProcessObservation{PID: 42, Exited: true}, nil)

	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("sandbox entry remains after active virtiofsd exit")
	}
	if entry.state != sandboxExited {
		t.Fatalf("state = %s, want exited", entry.state)
	}
}

func TestSuspendedVolumeObservationWithExitErrorCleansSandbox(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot, cidAllocator: NewCIDAllocator()}
	var cleanupCalls atomic.Int32
	sbx := &Sandbox{cleanup: NewCleanup(), sandboxID: "sandbox-a"}
	sbx.cleanup.Add(func(context.Context) error {
		cleanupCalls.Add(1)
		return nil
	})
	entry := &sandboxEntry{state: sandboxSuspended, sbx: sbx, cleanupDone: make(chan struct{})}
	m.sandboxes.Store("sandbox-a", entry)

	m.handleVolumeProcessObservation("sandbox-a", entry, volume.ProcessObservation{
		PID:    42,
		Exited: true,
		Cause:  errors.New("signal: killed"),
		Signal: "SIGKILL",
	}, nil)

	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}
	if entry.state != sandboxExited {
		t.Fatalf("state = %s, want exited", entry.state)
	}
}

func TestStoppingVolumeObservationDoesNotRepeatCleanup(t *testing.T) {
	m := &Manager{}
	var cleanupCalls atomic.Int32
	sbx := &Sandbox{cleanup: NewCleanup(), sandboxID: "sandbox-a"}
	sbx.cleanup.Add(func(context.Context) error {
		cleanupCalls.Add(1)
		return nil
	})
	entry := &sandboxEntry{state: sandboxStopping, sbx: sbx, cleanupDone: make(chan struct{})}
	m.sandboxes.Store("sandbox-a", entry)

	m.handleVolumeProcessObservation("sandbox-a", entry, volume.ProcessObservation{PID: 42, Exited: true}, nil)

	if got := cleanupCalls.Load(); got != 0 {
		t.Fatalf("cleanup calls = %d, want 0", got)
	}
	if entry.state != sandboxStopping {
		t.Fatalf("state = %s, want stopping", entry.state)
	}
}

func TestVolumeCleanupRunsWithoutHoldingEntryLock(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot, cidAllocator: NewCIDAllocator()}
	sbx := &Sandbox{cleanup: NewCleanup(), sandboxID: "sandbox-a"}
	entry := &sandboxEntry{state: sandboxReady, sbx: sbx, cleanupDone: make(chan struct{})}
	m.sandboxes.Store("sandbox-a", entry)
	lockAcquired := make(chan struct{})
	sbx.cleanup.Add(func(context.Context) error {
		entry.mu.Lock()
		entry.mu.Unlock()
		close(lockAcquired)
		return nil
	})
	done := make(chan struct{})
	go func() {
		m.handleVolumeProcessObservation("sandbox-a", entry, volume.ProcessObservation{PID: 42, Exited: true}, nil)
		close(done)
	}()

	select {
	case <-lockAcquired:
	case <-time.After(time.Second):
		t.Fatal("cleanup blocked acquiring entry.mu")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("volume cleanup did not finish")
	}
}

func TestVolumeExitDeleteAndVMMExitShareOneCleanupOwner(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot, cidAllocator: NewCIDAllocator()}
	var cleanupCalls atomic.Int32
	cleanupFailure := errors.New("volume cleanup failed")
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	sbx := &Sandbox{cleanup: NewCleanup(), sandboxID: "sandbox-a"}
	sbx.cleanup.Add(func(context.Context) error {
		if cleanupCalls.Add(1) == 1 {
			close(cleanupStarted)
		}
		<-releaseCleanup
		return cleanupFailure
	})
	entry := &sandboxEntry{state: sandboxReady, sbx: sbx, cleanupDone: make(chan struct{})}
	m.sandboxes.Store("sandbox-a", entry)

	volumeDone := make(chan struct{})
	go func() {
		m.handleVolumeProcessObservation("sandbox-a", entry, volume.ProcessObservation{PID: 42, Exited: true}, nil)
		close(volumeDone)
	}()
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("volume exit did not start cleanup")
	}

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- m.Delete(DeleteRequest{SandboxID: "sandbox-a"}) }()
	vmmDone := make(chan struct{})
	go func() {
		m.handleSandboxExit("sandbox-a", entry, "sandbox-a", sbx)
		close(vmmDone)
	}()
	close(releaseCleanup)

	select {
	case <-volumeDone:
	case <-time.After(time.Second):
		t.Fatal("volume cleanup owner did not finish")
	}
	select {
	case err := <-deleteDone:
		if !errors.Is(err, cleanupFailure) {
			t.Fatalf("Delete() error = %v, want shared cleanup failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Delete did not converge on existing cleanup")
	}
	select {
	case <-vmmDone:
	case <-time.After(time.Second):
		t.Fatal("VMM exit did not return after losing cleanup ownership")
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want exactly 1", got)
	}
}

func TestDeleteMarksStoppingBeforeVolumeExitAndRemainsCleanupOwner(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot, cidAllocator: NewCIDAllocator()}
	var cleanupCalls atomic.Int32
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	sbx := &Sandbox{cleanup: NewCleanup(), sandboxID: "sandbox-a"}
	sbx.cleanup.Add(func(context.Context) error {
		if cleanupCalls.Add(1) == 1 {
			close(cleanupStarted)
		}
		<-releaseCleanup
		return nil
	})
	entry := &sandboxEntry{state: sandboxReady, sbx: sbx, cleanupDone: make(chan struct{})}
	m.sandboxes.Store("sandbox-a", entry)

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- m.Delete(DeleteRequest{SandboxID: "sandbox-a"}) }()
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("Delete did not start cleanup")
	}
	entry.mu.Lock()
	stateDuringCleanup := entry.state
	entry.mu.Unlock()
	if stateDuringCleanup != sandboxStopping {
		t.Fatalf("state during Delete cleanup = %s, want stopping", stateDuringCleanup)
	}

	m.handleVolumeProcessObservation("sandbox-a", entry, volume.ProcessObservation{
		PID:    42,
		Exited: true,
		Cause:  errors.New("signal: killed"),
		Signal: "SIGKILL",
	}, nil)
	close(releaseCleanup)
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Delete cleanup did not finish")
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want exactly 1", got)
	}
}

func TestOldVolumeObservationCannotCleanupReplacementSandbox(t *testing.T) {
	m := &Manager{}
	var oldCleanupCalls atomic.Int32
	oldSandbox := &Sandbox{cleanup: NewCleanup(), sandboxID: "sandbox-a"}
	oldSandbox.cleanup.Add(func(context.Context) error {
		oldCleanupCalls.Add(1)
		return nil
	})
	oldEntry := &sandboxEntry{state: sandboxReady, sbx: oldSandbox, cleanupDone: make(chan struct{})}
	replacement := &sandboxEntry{state: sandboxReady, sbx: &Sandbox{sandboxID: "sandbox-a"}, cleanupDone: make(chan struct{})}
	m.sandboxes.Store("sandbox-a", replacement)

	m.handleVolumeProcessObservation("sandbox-a", oldEntry, volume.ProcessObservation{PID: 42, Exited: true}, nil)

	if got := oldCleanupCalls.Load(); got != 0 {
		t.Fatalf("old cleanup calls = %d, want 0", got)
	}
	actual, ok := m.sandboxes.Load("sandbox-a")
	if !ok || actual != replacement {
		t.Fatalf("replacement entry changed: %#v", actual)
	}
}

func TestHandleSandboxExitCleansSuspendedSandbox(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{
		boot:         boot,
		cidAllocator: NewCIDAllocator(),
	}
	sbx := &Sandbox{
		cleanup:   NewCleanup(),
		sandboxID: "sandbox-a",
	}
	entry := &sandboxEntry{state: sandboxSuspended, sbx: sbx}
	mapKey := "sandbox-a"
	m.sandboxes.Store(mapKey, entry)

	m.handleSandboxExit(mapKey, entry, "sandbox-a", sbx)

	if _, ok := m.sandboxes.Load(mapKey); ok {
		t.Fatal("suspended sandbox entry remains after VMM exit")
	}
	if len(boot.released) != 1 || boot.released[0].SandboxID != "sandbox-a" {
		t.Fatalf("released boot layouts = %#v", boot.released)
	}
}

func TestCheckpointCapturesRunningAndSuspendedSandbox(t *testing.T) {
	tests := []struct {
		name            string
		initialState    sandboxLifecycleState
		wantPauseBefore bool
	}{
		{name: "running", initialState: sandboxReady, wantPauseBefore: true},
		{name: "suspended", initialState: sandboxSuspended, wantPauseBefore: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := CapturedBootComponents{
				MemRootPath:  "/capture/mem",
				VMMName:      "cloud-hypervisor",
				MemorySizeMB: 512,
			}
			capture := &recordingCheckpointCapture{result: want}
			m, entry, sbx := checkpointTestManager(tt.initialState, capture)

			got, err := m.Checkpoint(CheckpointRequest{SandboxID: "sandbox-a"})
			if err != nil {
				t.Fatalf("Checkpoint() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Checkpoint() result = %#v, want %#v", got, want)
			}
			if entry.state != tt.initialState {
				t.Fatalf("entry state after checkpoint = %s, want %s", entry.state, tt.initialState)
			}
			if len(capture.requests) != 1 {
				t.Fatalf("capture requests = %d, want 1", len(capture.requests))
			}
			if capture.requests[0].Source != sbx {
				t.Fatalf("capture source = %T %p, want sandbox %p", capture.requests[0].Source, capture.requests[0].Source, sbx)
			}
			if capture.requests[0].PauseBefore != tt.wantPauseBefore {
				t.Fatalf("PauseBefore = %v, want %v", capture.requests[0].PauseBefore, tt.wantPauseBefore)
			}
		})
	}
}

func TestCheckpointCaptureErrorRestoresPreviousLifecycleState(t *testing.T) {
	errCapture := errors.New("capture failed")
	tests := []struct {
		name         string
		initialState sandboxLifecycleState
	}{
		{name: "running", initialState: sandboxReady},
		{name: "suspended", initialState: sandboxSuspended},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := &recordingCheckpointCapture{err: errCapture}
			m, entry, _ := checkpointTestManager(tt.initialState, capture)

			_, err := m.Checkpoint(CheckpointRequest{SandboxID: "sandbox-a"})
			if !errors.Is(err, errCapture) {
				t.Fatalf("Checkpoint() error = %v, want errors.Is(capture error)", err)
			}
			if entry.state != tt.initialState {
				t.Fatalf("entry state after capture error = %s, want %s", entry.state, tt.initialState)
			}
		})
	}
}

func TestCheckpointResumeFailureLeavesSandboxSuspended(t *testing.T) {
	errResume := errors.New("resume failed")
	capture := &recordingCheckpointCapture{err: errors.Join(ErrCheckpointResume, errResume)}
	m, entry, _ := checkpointTestManager(sandboxReady, capture)

	_, err := m.Checkpoint(CheckpointRequest{SandboxID: "sandbox-a"})
	if !errors.Is(err, ErrCheckpointResume) || !errors.Is(err, errResume) {
		t.Fatalf("Checkpoint() error = %v, want joined resume failure", err)
	}
	if entry.state != sandboxSuspended {
		t.Fatalf("entry state after resume failure = %s, want %s", entry.state, sandboxSuspended)
	}
}

type recordingCheckpointCapture struct {
	requests []RuntimeCaptureRequest
	result   CapturedBootComponents
	err      error
}

func (r *recordingCheckpointCapture) Capture(_ context.Context, req RuntimeCaptureRequest) (CapturedBootComponents, error) {
	r.requests = append(r.requests, req)
	return r.result, r.err
}

func checkpointTestManager(initialState sandboxLifecycleState, capture CheckpointCapture) (*Manager, *sandboxEntry, *Sandbox) {
	m := &Manager{checkpointCapture: capture, requestTimeout: time.Second}
	sbx := &Sandbox{
		sandboxID: "sandbox-a",
	}
	entry := &sandboxEntry{state: initialState, sbx: sbx}
	m.sandboxes.Store("sandbox-a", entry)
	return m, entry, sbx
}

type recordingBootPreparer struct {
	released   []ReleaseBootRequest
	releaseErr error
}

func (r *recordingBootPreparer) Prepare(context.Context, PrepareBootRequest) (PreparedBoot, error) {
	return PreparedBoot{}, nil
}

func (r *recordingBootPreparer) Release(_ context.Context, req ReleaseBootRequest) error {
	r.released = append(r.released, req)
	return r.releaseErr
}

func TestDeleteMissingSandboxReturnsNotFoundWithoutReleasingBootLayout(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot}

	if err := m.Delete(DeleteRequest{SandboxID: "sandbox-a"}); err == nil || err.Error() != "sandbox sandbox-a not found" {
		t.Fatalf("Delete() error = %v, want sandbox not found", err)
	}
	if len(boot.released) != 0 {
		t.Fatalf("released boot layouts = %#v, want none", boot.released)
	}
}

func TestCleanupStaleBootResourcesReleasesBootLayout(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot}

	if err := m.cleanupStaleBootResources(context.Background(), []string{"sandbox-a"}); err != nil {
		t.Fatalf("cleanupStaleBootResources() error = %v", err)
	}
	if len(boot.released) != 1 || boot.released[0].SandboxID != "sandbox-a" {
		t.Fatalf("released boot layouts = %#v", boot.released)
	}
}

func TestCleanupStaleBootResourcesReturnsBootReleaseError(t *testing.T) {
	wantErr := errors.New("release failed")
	boot := &recordingBootPreparer{releaseErr: wantErr}
	m := &Manager{boot: boot}

	if err := m.cleanupStaleBootResources(context.Background(), []string{"sandbox-a"}); !errors.Is(err, wantErr) {
		t.Fatalf("cleanupStaleBootResources() error = %v, want %v", err, wantErr)
	}
}

func TestReserveSandboxEntryRejectsSameSandboxWithoutWaiting(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	defer m.sandboxes.CompareAndDelete(key, entry)

	_, _, err = m.reserveSandboxEntry("sandbox-a")
	if err == nil {
		t.Fatal("same sandbox reserve succeeded while an entry already existed")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("same sandbox reserve error = %v, want already exists", err)
	}
}
