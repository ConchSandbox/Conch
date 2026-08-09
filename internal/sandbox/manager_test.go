package sandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestReserveSandboxEntrySerializesSameSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	defer m.sandboxes.CompareAndDelete(key, entry)

	done := make(chan error, 1)
	go func() {
		_, _, err := m.reserveSandboxEntry("sandbox-a")
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("same sandbox reserve completed while entry lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	entry.mu.Unlock()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("same sandbox reserve succeeded while an entry already existed")
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("same sandbox reserve error = %v, want already exists", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("same sandbox reserve did not unblock after entry lock was released")
	}
}
