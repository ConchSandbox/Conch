package sandbox

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/config"
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
	defer entry.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		otherKey, otherEntry, err := m.reserveSandboxEntry("sandbox-b")
		if err == nil {
			m.sandboxes.CompareAndDelete(otherKey, otherEntry)
			otherEntry.mu.Unlock()
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

func TestHandleSandboxExitCallsUnexpectedHandlerOnce(t *testing.T) {
	m, entry, sbx := newExitTestSandbox(func(context.Context) error { return nil })
	type exitResult struct {
		id  string
		err error
	}
	called := make(chan exitResult, 2)
	m.UnexpectedExitHandler = func(id string, err error) { called <- exitResult{id: id, err: err} }
	m.handleSandboxExit("sandbox-a", entry, "sandbox-a", sbx)
	m.handleSandboxExit("sandbox-a", entry, "sandbox-a", sbx)
	select {
	case result := <-called:
		if result.id != "sandbox-a" || result.err != nil {
			t.Fatalf("handler result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected-exit handler was not called")
	}
	select {
	case result := <-called:
		t.Fatalf("handler called more than once for %#v", result)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCreatePropagatesCallerCancellationToBootPreparation(t *testing.T) {
	oldWorkDir := config.WorkDir
	workDir, err := os.MkdirTemp("/tmp", "csb-")
	if err != nil {
		t.Fatal(err)
	}
	config.WorkDir = workDir
	t.Cleanup(func() {
		config.WorkDir = oldWorkDir
		_ = os.RemoveAll(workDir)
	})
	boot := &blockingBootPreparer{entered: make(chan struct{})}
	m := &Manager{
		boot:           boot,
		cidAllocator:   NewCIDAllocator(),
		requestTimeout: time.Hour,
		vmmBinaries:    map[string]string{"cloud-hypervisor": "/unused"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Create(ctx, CreateRequest{
			TemplateID: "sha256:template", VMMName: "cloud-hypervisor", SandboxID: "sandbox-a",
			VCPUNum: 1, VCPUMax: 1, RAMMB: 128, AgentToken: "token",
		})
		done <- err
	}()
	select {
	case <-boot.entered:
	case err := <-done:
		t.Fatalf("Create() returned before boot preparation: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Create() did not reach boot preparation")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Create() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Create() did not observe caller cancellation")
	}
}

func TestCreateUsesLiveBoundedContextForBootCleanupAfterCancellation(t *testing.T) {
	oldWorkDir := config.WorkDir
	config.WorkDir = t.TempDir()
	t.Cleanup(func() { config.WorkDir = oldWorkDir })

	ctx, cancel := context.WithCancel(context.Background())
	boot := &recordingBootPreparer{
		prepared:    PreparedBoot{Runtime: BootRuntime{Resume: true}},
		prepareHook: cancel,
	}
	m := &Manager{
		boot:           boot,
		cidAllocator:   NewCIDAllocator(),
		requestTimeout: time.Second,
		vmmBinaries:    map[string]string{"cloud-hypervisor": "/unused"},
	}

	_, err := m.Create(ctx, CreateRequest{
		TemplateID: "sha256:template", VMMName: "cloud-hypervisor", SandboxID: "sandbox-a",
		VCPUNum: 1, VCPUMax: 1, RAMMB: 128, AgentToken: "token",
	})
	if err == nil {
		t.Fatal("Create() error = nil")
	}
	if boot.releaseContextErr != nil {
		t.Fatalf("boot cleanup context error = %v, want nil", boot.releaseContextErr)
	}
	if !boot.releaseHasDeadline {
		t.Fatal("boot cleanup context has no deadline")
	}
}

func TestDeleteRemovesEntryAfterCleanupFailure(t *testing.T) {
	wantErr := errors.New("runtime cleanup failed")
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot, cidAllocator: NewCIDAllocator()}
	cleanup := NewCleanup()
	cleanup.Add(func(context.Context) error { return wantErr })
	sbx := &Sandbox{cleanup: cleanup, sandboxID: "sandbox-a"}
	entry := &sandboxEntry{state: sandboxReady, sbx: sbx}
	m.sandboxes.Store("sandbox-a", entry)

	if err := m.Delete(DeleteRequest{SandboxID: "sandbox-a"}); !errors.Is(err, wantErr) {
		t.Fatalf("Delete() error = %v, want %v", err, wantErr)
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("sandbox entry remains after one-shot cleanup failure")
	}
}

func TestUnexpectedExitRemovesEntryAndReportsCleanupError(t *testing.T) {
	wantErr := errors.New("release boot failed")
	boot := &recordingBootPreparer{releaseErrors: []error{wantErr}}
	m := &Manager{boot: boot, cidAllocator: NewCIDAllocator()}
	sbx := &Sandbox{cleanup: NewCleanup(), sandboxID: "sandbox-a"}
	entry := &sandboxEntry{state: sandboxReady, sbx: sbx}
	m.sandboxes.Store("sandbox-a", entry)
	called := make(chan error, 1)
	m.UnexpectedExitHandler = func(_ string, err error) { called <- err }

	m.handleSandboxExit("sandbox-a", entry, "sandbox-a", sbx)
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("sandbox entry remains after one-shot unexpected-exit cleanup")
	}
	select {
	case err := <-called:
		if !errors.Is(err, wantErr) {
			t.Fatalf("unexpected-exit cleanup error = %v, want %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected-exit handler was not called")
	}
}

func TestWaitForSandboxExitCleansSandboxOnVirtiofsExit(t *testing.T) {
	cleanupDone := make(chan struct{})
	m, entry, sbx := newExitTestSandbox(func(context.Context) error {
		close(cleanupDone)
		return nil
	})

	vmmExit := make(chan struct{})
	virtiofsExit := make(chan struct{})
	go func() {
		select {
		case <-vmmExit:
		case <-virtiofsExit:
		}
		m.handleSandboxExit("sandbox-a", entry, "sandbox-a", sbx)
	}()
	close(virtiofsExit)

	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("sandbox cleanup was not triggered after virtiofsd exit")
	}
	entry.mu.Lock()
	entry.mu.Unlock()
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("sandbox entry remains after virtiofsd exit")
	}
}

func TestWaitForSandboxExitDoesNotDuplicateDeleteCleanup(t *testing.T) {
	cleanupCalls := 0
	cleanupStarted := make(chan struct{})
	continueCleanup := make(chan struct{})
	m, entry, sbx := newExitTestSandbox(func(context.Context) error {
		cleanupCalls++
		close(cleanupStarted)
		<-continueCleanup
		return nil
	})

	vmmExit := make(chan struct{})
	virtiofsExit := make(chan struct{})
	go func() {
		select {
		case <-vmmExit:
		case <-virtiofsExit:
		}
		m.handleSandboxExit("sandbox-a", entry, "sandbox-a", sbx)
	}()
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- m.Delete(DeleteRequest{SandboxID: "sandbox-a"})
	}()

	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("Delete did not start sandbox cleanup")
	}
	close(virtiofsExit)
	close(continueCleanup)

	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Delete blocked after virtiofsd exit")
	}
	if cleanupCalls != 1 {
		t.Fatalf("sandbox cleanup calls = %d, want 1", cleanupCalls)
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("sandbox entry remains after Delete")
	}
}

func newExitTestSandbox(cleanup func(context.Context) error) (*Manager, *sandboxEntry, *Sandbox) {
	m := &Manager{boot: &recordingBootPreparer{}, cidAllocator: NewCIDAllocator()}
	sbx := &Sandbox{cleanup: NewCleanup(), sandboxID: "sandbox-a"}
	sbx.cleanup.Add(cleanup)
	entry := &sandboxEntry{state: sandboxReady, sbx: sbx}
	m.sandboxes.Store("sandbox-a", entry)
	return m, entry, sbx
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
	released           []ReleaseBootRequest
	releaseErr         error
	releaseErrors      []error
	prepared           PreparedBoot
	prepareHook        func()
	releaseContextErr  error
	releaseHasDeadline bool
}

func (r *recordingBootPreparer) Prepare(context.Context, PrepareBootRequest) (PreparedBoot, error) {
	if r.prepareHook != nil {
		r.prepareHook()
	}
	return r.prepared, nil
}

func (r *recordingBootPreparer) Release(ctx context.Context, req ReleaseBootRequest) error {
	r.released = append(r.released, req)
	r.releaseContextErr = ctx.Err()
	_, r.releaseHasDeadline = ctx.Deadline()
	if len(r.releaseErrors) > 0 {
		err := r.releaseErrors[0]
		r.releaseErrors = r.releaseErrors[1:]
		return err
	}
	return r.releaseErr
}

type blockingBootPreparer struct {
	entered chan struct{}
}

func (b *blockingBootPreparer) Prepare(ctx context.Context, _ PrepareBootRequest) (PreparedBoot, error) {
	close(b.entered)
	<-ctx.Done()
	return PreparedBoot{}, ctx.Err()
}

func (b *blockingBootPreparer) Release(context.Context, ReleaseBootRequest) error { return nil }

func TestDeleteMissingSandboxReturnsNotFoundWithoutReleasingBootLayout(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot}

	if err := m.Delete(DeleteRequest{SandboxID: "sandbox-a"}); !errors.Is(err, ErrNotFound) {
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
	defer entry.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, _, err := m.reserveSandboxEntry("sandbox-a")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("same sandbox reserve error = %v, want already exists", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("same sandbox reserve blocked behind the existing entry lock")
	}
}
