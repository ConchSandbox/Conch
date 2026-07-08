package snapshot

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

func TestSnapshotRemoveRejectsGroupRootWithoutCascade(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"rootfs": {
			Name: "rootfs",
			Labels: map[string]string{
				common.SnapshotLabelGroupMemRef: "mem",
				common.SnapshotLabelGroupVMRef:  "vm",
			},
		},
		"mem": {Name: "mem"},
		"vm":  {Name: "vm"},
	})

	if err := ensureSnapshotCanRemoveAlone(context.Background(), snapshotter, "rootfs"); err == nil {
		t.Fatal("rootfs remove error = nil, want cascade requirement")
	}
	if err := ensureSnapshotCanRemoveAlone(context.Background(), snapshotter, "mem"); err != nil {
		t.Fatalf("unlabeled mem remove error = %v, want no reverse-reference check", err)
	}
}

func TestSnapshotRemoveAllowsStandaloneSnapshot(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"rootfs": {Name: "rootfs"},
	})

	if err := ensureSnapshotCanRemoveAlone(context.Background(), snapshotter, "rootfs"); err != nil {
		t.Fatalf("ensureSnapshotCanRemoveAlone() error = %v", err)
	}
	if err := removeSnapshotKey(context.Background(), snapshotter, "rootfs"); err != nil {
		t.Fatalf("removeSnapshotKey() error = %v", err)
	}
	if len(snapshotter.removed) != 1 || snapshotter.removed[0] != "rootfs" {
		t.Fatalf("removed = %#v, want [rootfs]", snapshotter.removed)
	}
}

func TestSnapshotRemoveRejectsComponentLabelWithoutRootfs(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"mem": {
			Name: "mem",
			Labels: map[string]string{
				common.SnapshotLabelGroupID: "missing-rootfs",
			},
		},
	})

	if err := ensureSnapshotCanRemoveAlone(context.Background(), snapshotter, "mem"); err == nil {
		t.Fatal("component remove error = nil, want component relationship error")
	}
}

func TestSnapshotCascadeRootfsFailureKeepsComponentsForRetry(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"rootfs": {
			Name: "rootfs",
			Labels: map[string]string{
				common.SnapshotLabelGroupMemRef: "mem",
				common.SnapshotLabelGroupVMRef:  "vm",
			},
		},
		"mem": {Name: "mem"},
		"vm":  {Name: "vm"},
	})
	snapshotter.removeErrs = map[string]error{"rootfs": errdefs.ErrFailedPrecondition}

	if err := removeSnapshotCascade(context.Background(), snapshotter, "rootfs"); err == nil {
		t.Fatal("rootfs remove error = nil, want error")
	}
	if _, err := snapshotter.Stat(context.Background(), "rootfs"); err != nil {
		t.Fatalf("rootfs anchor was removed after rootfs failure: %v", err)
	}
	if containsString(snapshotter.removed, "mem") || containsString(snapshotter.removed, "vm") {
		t.Fatalf("removed = %#v, components should remain for retry", snapshotter.removed)
	}
}

func TestSnapshotCascadeRemovesRootfsBeforeComponents(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"rootfs": {
			Name: "rootfs",
			Labels: map[string]string{
				common.SnapshotLabelGroupMemRef: "mem",
				common.SnapshotLabelGroupVMRef:  "vm",
			},
		},
		"mem": {Name: "mem"},
		"vm":  {Name: "vm"},
	})

	if err := removeSnapshotCascade(context.Background(), snapshotter, "rootfs"); err != nil {
		t.Fatalf("removeSnapshotCascade() error = %v", err)
	}
	want := []string{"rootfs", "mem", "vm"}
	if len(snapshotter.removed) != len(want) {
		t.Fatalf("removed = %#v, want %#v", snapshotter.removed, want)
	}
	for i := range want {
		if snapshotter.removed[i] != want[i] {
			t.Fatalf("removed = %#v, want %#v", snapshotter.removed, want)
		}
	}
}

func TestSnapshotCascadeRejectsComponentEntry(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"rootfs": {
			Name: "rootfs",
			Labels: map[string]string{
				common.SnapshotLabelGroupMemRef: "mem",
				common.SnapshotLabelGroupVMRef:  "vm",
			},
		},
		"mem": {
			Name: "mem",
			Labels: map[string]string{
				common.SnapshotLabelGroupID: "rootfs",
			},
		},
		"vm": {Name: "vm"},
	})

	if err := removeSnapshotCascade(context.Background(), snapshotter, "mem"); err == nil {
		t.Fatal("removeSnapshotCascade() error = nil, want rootfs-entry requirement")
	}
}

func TestSnapshotRemoveNotFoundIsNoop(t *testing.T) {
	snapshotter := newFakeSnapshotter(nil)
	if err := ensureSnapshotCanRemoveAlone(context.Background(), snapshotter, "missing"); err != nil {
		t.Fatalf("ensureSnapshotCanRemoveAlone() error = %v", err)
	}
	if err := removeSnapshotCascade(context.Background(), snapshotter, "missing"); err != nil {
		t.Fatalf("removeSnapshotCascade() error = %v", err)
	}
}

func TestSnapshotMetaAnnotatesConchRelationFromLabels(t *testing.T) {
	tests := []struct {
		name    string
		info    snapshots.Info
		role    string
		groupID string
	}{
		{
			name: "group rootfs",
			info: snapshots.Info{
				Name: "rootfs",
				Labels: map[string]string{
					common.SnapshotLabelGroupMemRef: "mem",
					common.SnapshotLabelGroupVMRef:  "vm",
				},
			},
			role:    conchRoleRootfs,
			groupID: "rootfs",
		},
		{
			name: "mem component",
			info: snapshots.Info{
				Name: "mem",
				Labels: map[string]string{
					common.SnapshotLabelGroupID:       "rootfs",
					common.SnapshotLabelComponentKind: common.SnapshotComponentKindMem,
				},
			},
			role:    conchRoleMem,
			groupID: "rootfs",
		},
		{
			name: "vm component",
			info: snapshots.Info{
				Name: "vm",
				Labels: map[string]string{
					common.SnapshotLabelGroupID:       "rootfs",
					common.SnapshotLabelComponentKind: common.SnapshotComponentKindVM,
				},
			},
			role:    conchRoleVM,
			groupID: "rootfs",
		},
		{
			name: "component without known kind",
			info: snapshots.Info{
				Name: "component",
				Labels: map[string]string{
					common.SnapshotLabelGroupID: "rootfs",
				},
			},
			role:    conchRoleUnknown,
			groupID: "rootfs",
		},
		{
			name: "standalone",
			info: snapshots.Info{Name: "standalone"},
			role: conchRoleStandalone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := snapshotMeta(tt.info)
			if got.ConchRole != tt.role || got.GroupID != tt.groupID {
				t.Fatalf("snapshotMeta() relation = role=%q group_id=%q, want role=%q group_id=%q", got.ConchRole, got.GroupID, tt.role, tt.groupID)
			}
		})
	}
}

type fakeSnapshotter struct {
	infos      map[string]snapshots.Info
	removeErrs map[string]error
	removed    []string
}

func newFakeSnapshotter(infos map[string]snapshots.Info) *fakeSnapshotter {
	copied := make(map[string]snapshots.Info, len(infos))
	for key, info := range infos {
		copied[key] = info
	}
	return &fakeSnapshotter{infos: copied}
}

func (f *fakeSnapshotter) Stat(_ context.Context, key string) (snapshots.Info, error) {
	info, ok := f.infos[key]
	if !ok {
		return snapshots.Info{}, errdefs.ErrNotFound
	}
	return info, nil
}

func (f *fakeSnapshotter) Update(_ context.Context, info snapshots.Info, _ ...string) (snapshots.Info, error) {
	f.infos[info.Name] = info
	return info, nil
}

func (f *fakeSnapshotter) Usage(context.Context, string) (snapshots.Usage, error) {
	return snapshots.Usage{}, nil
}

func (f *fakeSnapshotter) Mounts(context.Context, string) ([]mount.Mount, error) {
	return nil, nil
}

func (f *fakeSnapshotter) Prepare(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (f *fakeSnapshotter) View(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (f *fakeSnapshotter) Commit(context.Context, string, string, ...snapshots.Opt) error {
	return nil
}

func (f *fakeSnapshotter) Remove(_ context.Context, key string) error {
	if err := f.removeErrs[key]; err != nil {
		return err
	}
	if _, ok := f.infos[key]; !ok {
		return errdefs.ErrNotFound
	}
	f.removed = append(f.removed, key)
	delete(f.infos, key)
	return nil
}

func (f *fakeSnapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, _ ...string) error {
	for _, info := range f.infos {
		if err := fn(ctx, info); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSnapshotter) Close() error {
	return nil
}
