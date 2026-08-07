package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/volume"
)

func TestVolumeHealthRelayInvokesSandboxExitLifecycle(t *testing.T) {
	errUnhealthy := errors.New("virtiofsd exited unexpectedly")
	ctx, cancel := context.WithCancelCause(context.Background())
	relay := newVolumeHealthRelay(cancel)

	boot := &recordingBootPreparer{}
	manager := &Manager{
		boot:         boot,
		cidAllocator: NewCIDAllocatorInDir(t.TempDir()),
	}
	var cleaned atomic.Bool
	sandbox := &Sandbox{
		cleanup:   NewCleanup(),
		namespace: "ns",
		sandboxID: "sandbox-a",
	}
	sandbox.cleanup.Add(func(context.Context) error {
		cleaned.Store(true)
		return nil
	})
	entry := &sandboxEntry{state: sandboxReady, sbx: sandbox}
	mapKey := sandboxMapKey("ns", "sandbox-a")
	manager.sandboxes.Store(mapKey, entry)

	if err := relay.activate(func(healthErr error) {
		manager.handleSandboxUnhealthy(mapKey, entry, "sandbox-a", sandbox, healthErr)
	}, nil); err != nil {
		t.Fatalf("activate() error = %v", err)
	}
	relay.report(errUnhealthy)

	if !errors.Is(context.Cause(ctx), errUnhealthy) {
		t.Fatalf("context cause = %v, want unhealthy error", context.Cause(ctx))
	}
	if !cleaned.Load() {
		t.Fatal("sandbox cleanup did not run after volume health failure")
	}
	if _, ok := manager.sandboxes.Load(mapKey); ok {
		t.Fatal("sandbox entry remains after volume health failure")
	}
	if len(boot.released) != 1 || boot.released[0].SandboxID != "sandbox-a" {
		t.Fatalf("released boot layouts = %#v", boot.released)
	}
}

func TestVolumeHealthRelayPropagatesFailureBeforeActivation(t *testing.T) {
	errFirst := errors.New("first volume failure")
	errSecond := errors.New("second volume failure")
	ctx, cancel := context.WithCancelCause(context.Background())
	relay := newVolumeHealthRelay(cancel)
	relay.report(errFirst)
	relay.report(errSecond)

	var called atomic.Bool
	err := relay.activate(func(error) {
		called.Store(true)
	}, nil)
	if !errors.Is(err, errFirst) {
		t.Fatalf("activate() error = %v, want first volume failure", err)
	}
	if called.Load() {
		t.Fatal("handler ran for a failure reported before activation")
	}
	if !errors.Is(relay.err(), errFirst) {
		t.Fatalf("relay.err() = %v, want first volume failure", relay.err())
	}
	if !errors.Is(context.Cause(ctx), errFirst) {
		t.Fatalf("context cause = %v, want first volume failure", context.Cause(ctx))
	}
}

func TestVolumeHealthRelayPublishesBeforeConcurrentCallback(t *testing.T) {
	errUnhealthy := errors.New("virtiofsd exited during activation")
	relay := newVolumeHealthRelay(nil)
	publishStarted := make(chan struct{})
	releasePublish := make(chan struct{})
	handlerCalled := make(chan error, 1)
	activateDone := make(chan error, 1)
	var published atomic.Bool

	go func() {
		activateDone <- relay.activate(func(err error) {
			if !published.Load() {
				handlerCalled <- errors.New("handler ran before readiness publication")
				return
			}
			handlerCalled <- err
		}, func() {
			close(publishStarted)
			<-releasePublish
			published.Store(true)
		})
	}()
	<-publishStarted
	reportDone := make(chan struct{})
	go func() {
		relay.report(errUnhealthy)
		close(reportDone)
	}()

	select {
	case err := <-handlerCalled:
		t.Fatalf("handler ran while readiness publication was blocked: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(releasePublish)
	if err := <-activateDone; err != nil {
		t.Fatalf("activate() error = %v", err)
	}
	select {
	case err := <-handlerCalled:
		if !errors.Is(err, errUnhealthy) {
			t.Fatalf("handler error = %v, want unhealthy error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent health report did not reach activated handler")
	}
	<-reportDone
}

func TestRehydrateWithoutVolumesDoesNotNeedHealthRelay(t *testing.T) {
	manager := &Manager{
		cidAllocator: NewCIDAllocatorInDir(t.TempDir()),
		attachSandbox: func(rec state.SandboxRecord, _ *netstack.Pool) (*Sandbox, error) {
			return &Sandbox{
				cleanup:   NewCleanup(),
				namespace: rec.Namespace,
				sandboxID: rec.SandboxID,
			}, nil
		},
	}
	record := state.SandboxRecord{
		Namespace: "ns",
		SandboxID: "sandbox-no-volumes",
		State:     state.SandboxReady,
	}

	restored, restoredIDs, err := manager.Rehydrate([]state.SandboxRecord{record})
	if err != nil {
		t.Fatalf("Rehydrate() error = %v", err)
	}
	if restored != 1 {
		t.Fatalf("Rehydrate() count = %d, want 1", restored)
	}
	if _, ok := restoredIDs[record.SandboxID]; !ok {
		t.Fatalf("restored sandbox IDs = %#v, want %s", restoredIDs, record.SandboxID)
	}
	mapKey := sandboxMapKey(record.Namespace, record.SandboxID)
	if _, ok := manager.sandboxes.Load(mapKey); !ok {
		t.Fatal("no-volume sandbox was not published")
	}
}

func TestRehydratedVolumeDeathUsesSandboxCleanupLifecycle(t *testing.T) {
	volumeManager := &healthTestVolumeManager{}
	boot := &recordingBootPreparer{}
	manager := &Manager{
		boot:          boot,
		cidAllocator:  NewCIDAllocatorInDir(t.TempDir()),
		volumeManager: volumeManager,
		attachSandbox: func(rec state.SandboxRecord, _ *netstack.Pool) (*Sandbox, error) {
			return &Sandbox{
				cleanup:   NewCleanup(),
				namespace: rec.Namespace,
				sandboxID: rec.SandboxID,
			}, nil
		},
	}
	record := state.SandboxRecord{
		Namespace: "ns",
		SandboxID: "sandbox-restored-volume",
		State:     state.SandboxReady,
		VolumeDevices: []state.VolumeDevice{{
			SandboxID: "sandbox-restored-volume",
			Namespace: "ns",
			Backend:   volume.DefaultBackend,
			PID:       1234,
			StartTime: 5678,
		}},
	}

	restored, _, err := manager.Rehydrate([]state.SandboxRecord{record})
	if err != nil {
		t.Fatalf("Rehydrate() error = %v", err)
	}
	if restored != 1 || volumeManager.restoreCallback == nil {
		t.Fatalf("restored = %d, callback installed = %v", restored, volumeManager.restoreCallback != nil)
	}
	healthErr := &volume.UnhealthyError{
		Backend:   volume.DefaultBackend,
		Namespace: record.Namespace,
		SandboxID: record.SandboxID,
		PID:       1234,
		Cause:     errors.New("restored virtiofsd exited"),
	}
	volumeManager.health = healthErr
	volumeManager.restoreCallback(healthErr)

	mapKey := sandboxMapKey(record.Namespace, record.SandboxID)
	if _, ok := manager.sandboxes.Load(mapKey); ok {
		t.Fatal("rehydrated sandbox entry remains after restored volume death")
	}
	if got := volumeManager.cleanupCalls.Load(); got != 1 {
		t.Fatalf("volume CleanupSandbox calls = %d, want 1", got)
	}
	if len(boot.released) != 1 || boot.released[0].SandboxID != record.SandboxID {
		t.Fatalf("released boot layouts = %#v", boot.released)
	}
}

func TestRehydrateRollsBackVolumeDeathBeforePublication(t *testing.T) {
	healthErr := &volume.UnhealthyError{
		Backend:   volume.DefaultBackend,
		Namespace: "ns",
		SandboxID: "sandbox-dies-restoring",
		PID:       1234,
		Cause:     errors.New("virtiofsd exited before publication"),
	}
	volumeManager := &healthTestVolumeManager{restoreHealthDuringCall: healthErr}
	boot := &recordingBootPreparer{}
	manager := &Manager{
		boot:          boot,
		cidAllocator:  NewCIDAllocatorInDir(t.TempDir()),
		volumeManager: volumeManager,
		attachSandbox: func(rec state.SandboxRecord, _ *netstack.Pool) (*Sandbox, error) {
			return &Sandbox{
				cleanup:   NewCleanup(),
				namespace: rec.Namespace,
				sandboxID: rec.SandboxID,
			}, nil
		},
	}
	record := state.SandboxRecord{
		Namespace: "ns",
		SandboxID: "sandbox-dies-restoring",
		State:     state.SandboxReady,
		VolumeDevices: []state.VolumeDevice{{
			SandboxID: "sandbox-dies-restoring",
			Backend:   volume.DefaultBackend,
			PID:       1234,
			StartTime: 5678,
		}},
	}

	restored, restoredIDs, err := manager.Rehydrate([]state.SandboxRecord{record})
	if !errors.Is(err, volume.ErrBackendUnhealthy) {
		t.Fatalf("Rehydrate() error = %v, want pre-publication volume health", err)
	}
	if restored != 0 || len(restoredIDs) != 0 {
		t.Fatalf("restored = %d, IDs = %#v; want full rollback", restored, restoredIDs)
	}
	if _, ok := manager.sandboxes.Load(sandboxMapKey(record.Namespace, record.SandboxID)); ok {
		t.Fatal("sandbox was published after pre-publication volume death")
	}
	if got := volumeManager.cleanupCalls.Load(); got != 1 {
		t.Fatalf("volume CleanupSandbox calls = %d, want 1", got)
	}
	if len(boot.released) != 1 || boot.released[0].SandboxID != record.SandboxID {
		t.Fatalf("released boot layouts = %#v", boot.released)
	}
}

func TestLifecycleOperationsReturnRetainedVolumeHealthPromptly(t *testing.T) {
	healthErr := &volume.UnhealthyError{
		Backend:   volume.DefaultBackend,
		Namespace: "ns",
		SandboxID: "sandbox-dead-volume",
		PID:       1234,
		Cause:     errors.New("virtiofsd exited"),
	}
	volumeManager := &healthTestVolumeManager{health: healthErr}
	manager := &Manager{volumeManager: volumeManager, requestTimeout: time.Second}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "suspend", run: func() error {
			return manager.Suspend(LifecycleRequest{Namespace: "ns", SandboxID: "sandbox-dead-volume"})
		}},
		{name: "resume", run: func() error {
			return manager.Resume(LifecycleRequest{Namespace: "ns", SandboxID: "sandbox-dead-volume"})
		}},
		{name: "checkpoint", run: func() error {
			_, err := manager.Checkpoint(CheckpointRequest{Namespace: "ns", SandboxID: "sandbox-dead-volume"})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := time.Now()
			err := tt.run()
			if !errors.Is(err, volume.ErrBackendUnhealthy) {
				t.Fatalf("operation error = %v, want retained volume health", err)
			}
			if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
				t.Fatalf("operation returned health after %s, want prompt failure", elapsed)
			}
		})
	}
	if got := volumeManager.checkCalls.Load(); got != int32(len(tests)) {
		t.Fatalf("CheckSandboxHealth calls = %d, want %d", got, len(tests))
	}
}

func TestDeleteAfterAutomaticVolumeCleanupConvergesAndClearsHealth(t *testing.T) {
	volumeManager := &healthTestVolumeManager{health: &volume.UnhealthyError{
		Backend:   volume.DefaultBackend,
		Namespace: "ns",
		SandboxID: "already-cleaned",
		PID:       1234,
		Cause:     errors.New("virtiofsd exited"),
	}}
	manager := &Manager{volumeManager: volumeManager}
	req := DeleteRequest{Namespace: "ns", SandboxID: "already-cleaned"}

	if err := manager.Delete(req); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got := volumeManager.checkCalls.Load(); got != 1 {
		t.Fatalf("CheckSandboxHealth calls = %d, want 1", got)
	}
	if got := volumeManager.clearCalls.Load(); got != 1 {
		t.Fatalf("ClearSandboxHealth calls = %d, want 1", got)
	}
}

func TestDeleteUnknownSandboxWithoutHealthPreservesNotFound(t *testing.T) {
	volumeManager := &healthTestVolumeManager{}
	manager := &Manager{volumeManager: volumeManager}

	err := manager.Delete(DeleteRequest{Namespace: "ns", SandboxID: "unknown"})
	if err == nil || !strings.Contains(err.Error(), "sandbox unknown not found") {
		t.Fatalf("Delete() error = %v, want original not-found error", err)
	}
	if got := volumeManager.checkCalls.Load(); got != 1 {
		t.Fatalf("CheckSandboxHealth calls = %d, want 1", got)
	}
	if got := volumeManager.clearCalls.Load(); got != 0 {
		t.Fatalf("ClearSandboxHealth calls = %d, want 0", got)
	}
}

func TestDeleteLoadedEntryRemovedByUnhealthyCleanupClearsHealth(t *testing.T) {
	healthErr := &volume.UnhealthyError{
		Backend:   volume.DefaultBackend,
		Namespace: "ns",
		SandboxID: "stale-entry",
		PID:       1234,
		Cause:     errors.New("virtiofsd exited"),
	}
	volumeManager := &healthTestVolumeManager{health: healthErr}
	manager := &Manager{volumeManager: volumeManager}
	entry := &sandboxEntry{state: sandboxReady, sbx: &Sandbox{sandboxID: "stale-entry"}}

	// Model Delete having loaded entry before automatic unhealthy cleanup
	// removed it from the map, then acquiring entry.mu afterward.
	if err := manager.deleteLoadedSandbox("ns", sandboxMapKey("ns", "stale-entry"), "stale-entry", entry); err != nil {
		t.Fatalf("deleteLoadedSandbox() error = %v", err)
	}
	if got := volumeManager.checkCalls.Load(); got != 1 {
		t.Fatalf("CheckSandboxHealth calls = %d, want 1", got)
	}
	if got := volumeManager.clearCalls.Load(); got != 1 {
		t.Fatalf("ClearSandboxHealth calls = %d, want 1", got)
	}
}

func TestDeleteLoadedStaleEntryWithoutHealthKeepsConvergedSemantics(t *testing.T) {
	volumeManager := &healthTestVolumeManager{}
	manager := &Manager{volumeManager: volumeManager}
	entry := &sandboxEntry{state: sandboxReady, sbx: &Sandbox{sandboxID: "stale-entry"}}

	if err := manager.deleteLoadedSandbox("ns", sandboxMapKey("ns", "stale-entry"), "stale-entry", entry); err != nil {
		t.Fatalf("deleteLoadedSandbox() error = %v", err)
	}
	if got := volumeManager.clearCalls.Load(); got != 0 {
		t.Fatalf("ClearSandboxHealth calls = %d, want 0", got)
	}
}

func TestActivationFailureCleansTransferredVolumeExactlyOnce(t *testing.T) {
	volumeManager := &healthTestVolumeManager{}
	sandbox := &Sandbox{cleanup: NewCleanup(), namespace: "ns", sandboxID: "sandbox-activation-failure"}
	devices := []volume.Device{{SandboxID: sandbox.sandboxID, Namespace: sandbox.namespace}}
	needsFallbackCleanup := true
	transferSandboxVolumeCleanup(sandbox, volumeManager, sandbox.namespace, sandbox.sandboxID, devices, &needsFallbackCleanup)
	if needsFallbackCleanup {
		t.Fatal("volume cleanup ownership was not transferred to sandbox cleanup")
	}

	relay := newVolumeHealthRelay(nil)
	healthErr := errors.New("virtiofsd exited before activation")
	relay.report(healthErr)
	if err := relay.activate(nil, nil); !errors.Is(err, healthErr) {
		t.Fatalf("activate() error = %v, want pre-activation health", err)
	}
	if err := sandbox.Close(context.Background()); err != nil {
		t.Fatalf("sandbox Close() error = %v", err)
	}
	if needsFallbackCleanup {
		_ = volumeManager.CleanupSandbox(sandbox.namespace, sandbox.sandboxID, devices)
	}
	if got := volumeManager.cleanupCalls.Load(); got != 1 {
		t.Fatalf("CleanupSandbox calls = %d, want exactly 1", got)
	}
}

type healthTestVolumeManager struct {
	health                  error
	restoreCallback         func(error)
	restoreHealthDuringCall error
	checkCalls              atomic.Int32
	clearCalls              atomic.Int32
	cleanupCalls            atomic.Int32
}

func (m *healthTestVolumeManager) PrepareSandboxWithHealth(string, string, []volume.Mount, func(error)) ([]volume.Device, error) {
	return nil, nil
}

func (m *healthTestVolumeManager) RestoreSandboxWithHealth(_ string, _ string, _ []volume.Device, onUnhealthy func(error)) error {
	m.restoreCallback = onUnhealthy
	if m.restoreHealthDuringCall != nil {
		onUnhealthy(m.restoreHealthDuringCall)
	}
	return nil
}

func (m *healthTestVolumeManager) CleanupSandbox(string, string, []volume.Device) error {
	m.cleanupCalls.Add(1)
	return nil
}

func (m *healthTestVolumeManager) CheckSandboxHealth(string, string) error {
	m.checkCalls.Add(1)
	return m.health
}

func (m *healthTestVolumeManager) ClearSandboxHealth(string, string) {
	m.clearCalls.Add(1)
	m.health = nil
}
