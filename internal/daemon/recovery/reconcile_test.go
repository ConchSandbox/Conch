package recovery

import (
	"context"
	"testing"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/daemon/state"
)

type fakeLeaseClient struct {
	seen map[string]string
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
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "default",
		State:          state.SandboxReady,
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
}

func TestReconcileMarksMissingViewSnapshotUnknown(t *testing.T) {
	ctx := context.Background()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	if err := store.UpsertViewSnapshot(ctx, state.ViewSnapshotRecord{
		Namespace:        "default",
		ParentSnapshotID: "parent-1",
		ViewSnapshotKey:  "view-1",
		MountPoint:       t.TempDir() + "/missing",
		State:            state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertViewSnapshot() error = %v", err)
	}

	result, err := Reconcile(ctx, Config{
		Store:            store,
		LeaseClient:      &fakeLeaseClient{},
		DefaultNamespace: "default",
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.ViewSnapshotsMarked != 1 {
		t.Fatalf("ViewSnapshotsMarked = %d, want 1", result.ViewSnapshotsMarked)
	}
	views, err := store.ListViewSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListViewSnapshots() error = %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1", len(views))
	}
	if views[0].State != state.SandboxUnknown {
		t.Fatalf("view.State = %q, want %q", views[0].State, state.SandboxUnknown)
	}
}
