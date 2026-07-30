package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/template"
)

func TestReserveSandboxEntryDoesNotBlockDifferentSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("ns", "sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	defer m.sandboxes.CompareAndDelete(key, entry)
	defer entry.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		otherKey, otherEntry, err := m.reserveSandboxEntry("ns", "sandbox-b")
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

func TestUpdateNetworkRejectsInvalidPolicy(t *testing.T) {
	m := &Manager{requestTimeout: time.Second}
	m.sandboxes.Store(sandboxMapKey("ns", "sandbox-a"), &sandboxEntry{
		state: sandboxReady,
		sbx:   &Sandbox{slot: &netstack.Slot{Key: "slot-a"}},
	})

	err := m.UpdateNetwork(context.Background(), NetworkUpdateRequest{
		Namespace: "ns",
		SandboxID: "sandbox-a",
		Network:   json.RawMessage(`{`),
	})
	if err == nil || !strings.Contains(err.Error(), "invalid sandbox network config") {
		t.Fatalf("UpdateNetwork() error = %v, want invalid network config", err)
	}
}

func TestUpdateNetworkForwardsPolicyToPool(t *testing.T) {
	original := updateSandboxNetworkPolicy
	defer func() { updateSandboxNetworkPolicy = original }()

	slot := &netstack.Slot{Key: "slot-a"}
	var gotSandboxID string
	var gotPolicy *runtimeapi.SandboxNetworkConfig
	type contextKey struct{}
	updateCtx := context.WithValue(context.Background(), contextKey{}, "update")
	updateSandboxNetworkPolicy = func(_ *netstack.Pool, ctx context.Context, gotSlot *netstack.Slot, sandboxID string, policy *runtimeapi.SandboxNetworkConfig) error {
		if gotSlot != slot {
			t.Fatalf("slot = %#v, want %#v", gotSlot, slot)
		}
		if ctx.Value(contextKey{}) != "update" {
			t.Fatal("UpdateNetwork() did not forward its context")
		}
		gotSandboxID = sandboxID
		gotPolicy = policy
		return nil
	}

	m := &Manager{requestTimeout: time.Second}
	m.sandboxes.Store(sandboxMapKey("ns", "sandbox-a"), &sandboxEntry{
		state: sandboxReady,
		sbx:   &Sandbox{slot: slot},
	})

	err := m.UpdateNetwork(updateCtx, NetworkUpdateRequest{
		Namespace: "ns",
		SandboxID: "sandbox-a",
		Network:   json.RawMessage(`{"denyOut":["198.51.100.1"]}`),
	})
	if err != nil {
		t.Fatalf("UpdateNetwork() error = %v", err)
	}
	if gotSandboxID != "sandbox-a" || !reflect.DeepEqual(gotPolicy.DenyOut, []string{"198.51.100.1"}) {
		t.Fatalf("forwarded sandbox/policy = %q/%#v", gotSandboxID, gotPolicy)
	}
}

func TestHandleSandboxExitCleansSuspendedSandbox(t *testing.T) {
	templateAdapter := &recordingTemplate{}
	m := &Manager{
		template:     templateAdapter,
		cidAllocator: NewCIDAllocatorInDir(t.TempDir()),
	}
	sbx := &Sandbox{
		cleanup:   NewCleanup(),
		namespace: "ns",
		sandboxID: "sandbox-a",
	}
	entry := &sandboxEntry{state: sandboxSuspended, sbx: sbx}
	mapKey := sandboxMapKey("ns", "sandbox-a")
	m.sandboxes.Store(mapKey, entry)

	m.handleSandboxExit(mapKey, entry, "sandbox-a", sbx)

	if _, ok := m.sandboxes.Load(mapKey); ok {
		t.Fatal("suspended sandbox entry remains after VMM exit")
	}
	if len(templateAdapter.released) != 1 || templateAdapter.released[0].SandboxID != "sandbox-a" {
		t.Fatalf("released boot layouts = %#v", templateAdapter.released)
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

			got, err := m.Checkpoint(CheckpointRequest{Namespace: "ns", SandboxID: "sandbox-a"})
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

			_, err := m.Checkpoint(CheckpointRequest{Namespace: "ns", SandboxID: "sandbox-a"})
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

	_, err := m.Checkpoint(CheckpointRequest{Namespace: "ns", SandboxID: "sandbox-a"})
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
		namespace: "ns",
		sandboxID: "sandbox-a",
	}
	entry := &sandboxEntry{state: initialState, sbx: sbx}
	m.sandboxes.Store(sandboxMapKey("ns", "sandbox-a"), entry)
	return m, entry, sbx
}

type recordingTemplate struct {
	released []template.ReleaseSandboxBootRequest
}

func (r *recordingTemplate) PrepareSandboxBoot(context.Context, template.PrepareSandboxBootRequest) (template.PreparedSandboxBoot, error) {
	return template.PreparedSandboxBoot{}, nil
}

func (r *recordingTemplate) ReleaseSandboxBoot(_ context.Context, req template.ReleaseSandboxBootRequest) error {
	r.released = append(r.released, req)
	return nil
}

func TestReserveSandboxEntrySerializesSameSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("ns", "sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	defer m.sandboxes.CompareAndDelete(key, entry)

	done := make(chan error, 1)
	go func() {
		_, _, err := m.reserveSandboxEntry("ns", "sandbox-a")
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("same sandbox reserve completed while entry lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	entry.state = sandboxReady
	entry.mu.Unlock()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("same sandbox reserve succeeded while an entry already existed")
		}
		if !strings.Contains(err.Error(), "ready") {
			t.Fatalf("same sandbox reserve error = %v, want ready state", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("same sandbox reserve did not unblock after entry lock was released")
	}
}

func TestLifecycleOperationsRejectCreatingSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("ns", "sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	entry.mu.Unlock()
	defer m.sandboxes.CompareAndDelete(key, entry)

	err = m.Delete(DeleteRequest{Namespace: "ns", SandboxID: "sandbox-a"})
	if err == nil {
		t.Fatal("Delete() succeeded for creating sandbox")
	}
	if !strings.Contains(err.Error(), "creating") {
		t.Fatalf("Delete() error = %v, want creating state", err)
	}

	_, err = m.Checkpoint(CheckpointRequest{Namespace: "ns", SandboxID: "sandbox-a"})
	if err == nil {
		t.Fatal("Checkpoint() succeeded for creating sandbox")
	}
	if !strings.Contains(err.Error(), "creating") {
		t.Fatalf("Checkpoint() error = %v, want creating state", err)
	}
}
