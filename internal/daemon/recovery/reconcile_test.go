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
	cleanupID  map[string]struct{}
	cleanupErr error
}

func (f *fakeSandboxRehydrator) Rehydrate(records []state.SandboxRecord) (int, map[string]struct{}, error) {
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

func TestReconcileDowngradesUnverifiableSandboxAndContainer(t *testing.T) {
	ctx := context.Background()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "default",
		State:          state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	if err := store.UpsertContainer(ctx, state.ContainerRecord{
		ContainerID:  "container-1",
		PodSandboxID: "pod-1",
		State:        state.ContainerRunning,
	}); err != nil {
		t.Fatalf("UpsertContainer() error = %v", err)
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
	if result.ContainersDowngraded != 1 {
		t.Fatalf("ContainersDowngraded = %d, want 1", result.ContainersDowngraded)
	}
	if result.RuntimeLeasesChecked != 1 {
		t.Fatalf("RuntimeLeasesChecked = %d, want 1", result.RuntimeLeasesChecked)
	}
	if leases.seen["default"] != containerdclient.RuntimeLeaseID("default") {
		t.Fatalf("runtime lease = %q, want %q", leases.seen["default"], containerdclient.RuntimeLeaseID("default"))
	}

	sandbox, err := store.GetSandbox(ctx, "pod-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if sandbox.State != state.SandboxNotReady {
		t.Fatalf("sandbox.State = %q, want %q", sandbox.State, state.SandboxNotReady)
	}
	if sandbox.LeaseID != containerdclient.RuntimeLeaseID("default") {
		t.Fatalf("sandbox.LeaseID = %q, want runtime lease", sandbox.LeaseID)
	}

	container, err := store.GetContainer(ctx, "container-1")
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if container.State != state.ContainerUnknown {
		t.Fatalf("container.State = %q, want %q", container.State, state.ContainerUnknown)
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
		PodSandboxID:    "pod-1",
		ConchSandboxID:  "sandbox-1",
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
	rec, err := store.GetSandbox(ctx, "pod-1")
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
		PodSandboxID:    "pod-1",
		ConchSandboxID:  "sandbox-1",
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
		PodSandboxID:    "pod-ready",
		ConchSandboxID:  "sandbox-ready",
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
	if _, ok := rehydrator.cleanupID["sandbox-ready"]; !ok {
		t.Fatalf("cleanup ids = %#v, want sandbox-ready", rehydrator.cleanupID)
	}
}
