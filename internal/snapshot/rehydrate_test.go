package snapshot

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"

	"github.com/openeuler/Conch/internal/daemon/state"
)

func TestRehydrateRuntimeStateSkipsAliasesForUnrestoredViews(t *testing.T) {
	ctx := context.Background()
	mountPoint := t.TempDir()
	missingMountPoint := t.TempDir() + "/missing"

	gServer = server{
		snt:              fakeSnapshotter{},
		activeSnapshots:  make(map[string]map[string]*snapshots.Info),
		activeRootfsPmem: make(map[string]map[string][]string),
		viewMgr: &viewManager{
			viewMounts:  make(map[string]map[string]*viewMountRef),
			viewAliases: make(map[string]map[string]string),
		},
	}
	t.Cleanup(func() {
		gServer = server{}
	})

	result, err := RehydrateRuntimeState(ctx, nil, []state.ViewSnapshotRecord{
		{
			Namespace:        "default",
			ParentSnapshotID: "parent-ready",
			ViewSnapshotKey:  "view-ready",
			MountPoint:       mountPoint,
			RefCount:         1,
			State:            state.SandboxReady,
		},
		{
			Namespace:        "default",
			ParentSnapshotID: "parent-missing",
			ViewSnapshotKey:  "view-missing",
			MountPoint:       missingMountPoint,
			RefCount:         1,
			State:            state.SandboxReady,
		},
	}, []state.ViewAliasRecord{
		{
			Namespace:        "default",
			AliasKey:         "alias-ready",
			ParentSnapshotID: "parent-ready",
		},
		{
			Namespace:        "default",
			AliasKey:         "alias-missing",
			ParentSnapshotID: "parent-missing",
		},
	})
	if err == nil {
		t.Fatalf("RehydrateRuntimeState() error = nil, want missing mount error")
	}
	if result.ViewMounts != 1 {
		t.Fatalf("ViewMounts = %d, want 1", result.ViewMounts)
	}
	if result.ViewAliases != 1 {
		t.Fatalf("ViewAliases = %d, want 1", result.ViewAliases)
	}
	if parent, ok := gServer.viewMgr.getViewAlias("default", "alias-ready"); !ok || parent != "parent-ready" {
		t.Fatalf("ready alias = (%q, %v), want parent-ready true", parent, ok)
	}
	if parent, ok := gServer.viewMgr.getViewAlias("default", "alias-missing"); ok {
		t.Fatalf("missing alias restored to %q, want absent", parent)
	}
}

type fakeSnapshotter struct{}

func (fakeSnapshotter) Prepare(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (fakeSnapshotter) View(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (fakeSnapshotter) Mounts(context.Context, string, string) ([]mount.Mount, error) {
	return nil, nil
}

func (fakeSnapshotter) Commit(context.Context, string, string, string, ...snapshots.Opt) error {
	return nil
}

func (fakeSnapshotter) Update(context.Context, string, snapshots.Info, ...string) (snapshots.Info, error) {
	return snapshots.Info{}, nil
}

func (fakeSnapshotter) Remove(context.Context, string, string) error {
	return nil
}

func (fakeSnapshotter) Stat(context.Context, string, string) (snapshots.Info, error) {
	return snapshots.Info{}, nil
}

func (fakeSnapshotter) List(context.Context, string, map[string]*snapshots.Info, ...string) error {
	return nil
}

func (fakeSnapshotter) ListNamespaces(context.Context) ([]string, error) {
	return nil, nil
}

func (fakeSnapshotter) Close() error {
	return nil
}
