package recovery

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/daemon/state"
)

type fakeLeaseClient struct {
	seen map[string]string
}

type fakeSandboxRehydrator struct {
	count      int
	restoredID map[string]struct{}
	err        error
	records    []state.SandboxRecord
	cleanupID  map[string]struct{}
	cleanupErr error
}

func (f *fakeSandboxRehydrator) Rehydrate(records []state.SandboxRecord) (int, map[string]struct{}, error) {
	f.records = append([]state.SandboxRecord(nil), records...)
	return f.count, f.restoredID, f.err
}

func (f *fakeSandboxRehydrator) CleanupAssignedWithoutReadySandbox(ids map[string]struct{}) error {
	f.cleanupID = ids
	return f.cleanupErr
}

func (f *fakeLeaseClient) WithRuntimeLease(ctx context.Context, namespace, leaseID string) (context.Context, string, error) {
	if f.seen == nil {
		f.seen = make(map[string]string)
	}
	if leaseID == "" {
		leaseID = containerdclient.RuntimeLeaseID(namespace)
	}
	f.seen[namespace] = leaseID
	return ctx, leaseID, nil
}

func TestReconcileDowngradesUnverifiableSandbox(t *testing.T) {
	ctx := context.Background()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID: "sandbox-1",
		Namespace: "default",
		State:     state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	leases := &fakeLeaseClient{}
	result, err := Reconcile(ctx, Config{
		Store:            store,
		LeaseClient:      leases,
		DefaultNamespace: "default",
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.SandboxesDowngraded != 1 {
		t.Fatalf("SandboxesDowngraded = %d, want 1", result.SandboxesDowngraded)
	}
	if result.RuntimeLeasesChecked != 1 {
		t.Fatalf("RuntimeLeasesChecked = %d, want 1", result.RuntimeLeasesChecked)
	}
	if leases.seen["default"] != containerdclient.RuntimeLeaseID("default") {
		t.Fatalf("runtime lease = %q, want %q", leases.seen["default"], containerdclient.RuntimeLeaseID("default"))
	}

	sandbox, err := store.GetSandbox(ctx, "sandbox-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if sandbox.State != state.SandboxNotReady {
		t.Fatalf("sandbox.State = %q, want %q", sandbox.State, state.SandboxNotReady)
	}
	if sandbox.LeaseID != containerdclient.RuntimeLeaseID("default") {
		t.Fatalf("sandbox.LeaseID = %q, want runtime lease", sandbox.LeaseID)
	}
}

func TestReconcileDowngradesSandboxWithMissingMount(t *testing.T) {
	ctx := context.Background()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID:       "sandbox-1",
		Namespace:       "default",
		State:           state.SandboxReady,
		VMMSocketPath:   t.TempDir() + "/vmm.sock",
		RootfsMount:     t.TempDir() + "/missing-rootfs",
		MemMount:        t.TempDir() + "/missing-mem",
		VMMount:         t.TempDir() + "/missing-vm",
		VsockSocketPath: t.TempDir() + "/vsock.sock",
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	result, err := Reconcile(ctx, Config{
		Store:            store,
		LeaseClient:      &fakeLeaseClient{},
		DefaultNamespace: "default",
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.SandboxesDowngraded != 1 {
		t.Fatalf("SandboxesDowngraded = %d, want 1", result.SandboxesDowngraded)
	}
	rec, err := store.GetSandbox(ctx, "sandbox-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec.State != state.SandboxNotReady {
		t.Fatalf("sandbox.State = %q, want %q", rec.State, state.SandboxNotReady)
	}
	if !strings.Contains(rec.LastError, "rootfs mount is missing") {
		t.Fatalf("LastError = %q, want missing rootfs mount reason", rec.LastError)
	}
}

func TestReconcileReturnsSandboxRehydrateError(t *testing.T) {
	ctx := context.Background()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	nsPath := t.TempDir()
	vsockPath := nsPath + "/vsock.sock"
	if err := os.WriteFile(vsockPath, []byte{}, 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID:       "sandbox-1",
		Namespace:       "default",
		State:           state.SandboxReady,
		NetworkNS:       nsPath,
		VsockSocketPath: vsockPath,
		IP:              "10.12.0.2",
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	wantErr := errors.New("rehydrate failed")
	rehydrator := &fakeSandboxRehydrator{
		err:        wantErr,
		restoredID: map[string]struct{}{"sandbox-1": {}},
	}
	result, err := Reconcile(ctx, Config{
		Store:             store,
		LeaseClient:       &fakeLeaseClient{},
		SandboxRehydrator: rehydrator,
		DefaultNamespace:  "default",
	})
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("Reconcile() error = %v, want %v", err, wantErr)
	}
	if result.RehydrateErrors != 1 {
		t.Fatalf("RehydrateErrors = %d, want 1", result.RehydrateErrors)
	}
	if _, ok := rehydrator.cleanupID["sandbox-1"]; !ok {
		t.Fatalf("CleanupAssignedWithoutReadySandbox() ids = %v, want sandbox-1", rehydrator.cleanupID)
	}
}

func TestReconcileCleansStaleAssignedNetworkSlotsAfterRehydrate(t *testing.T) {
	ctx := context.Background()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	nsPath := t.TempDir()
	vsockPath := nsPath + "/vsock.sock"
	if err := os.WriteFile(vsockPath, []byte{}, 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID:       "sandbox-ready",
		Namespace:       "default",
		State:           state.SandboxReady,
		NetworkNS:       nsPath,
		VsockSocketPath: vsockPath,
		IP:              "10.12.0.2",
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	rehydrator := &fakeSandboxRehydrator{
		count:      1,
		restoredID: map[string]struct{}{"sandbox-ready": {}},
	}
	result, err := Reconcile(ctx, Config{
		Store:             store,
		LeaseClient:       &fakeLeaseClient{},
		SandboxRehydrator: rehydrator,
		DefaultNamespace:  "default",
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.SandboxesRehydrated != 1 {
		t.Fatalf("SandboxesRehydrated = %d, want 1", result.SandboxesRehydrated)
	}
	if len(rehydrator.records) != 1 || rehydrator.records[0].SandboxID != "sandbox-ready" {
		t.Fatalf("rehydrated records = %#v, want sandbox-ready", rehydrator.records)
	}
	if _, ok := rehydrator.cleanupID["sandbox-ready"]; !ok {
		t.Fatalf("cleanup ids = %#v, want sandbox-ready", rehydrator.cleanupID)
	}
}
