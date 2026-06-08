package snapshot

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

func TestSnapshotRemoveRejectsAssociatedGroupWithoutCascade(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"rootfs": {
			Name: "rootfs",
			Labels: map[string]string{
				common.SnapshotLabelMemSnapshot: "mem",
				common.SnapshotLabelVMSnapshot:  "vm",
			},
		},
		"mem": {Name: "mem"},
		"vm":  {Name: "vm"},
	})

	if err := ensureSnapshotCanRemoveAlone(context.Background(), snapshotter, "rootfs"); err == nil {
		t.Fatal("rootfs remove error = nil, want cascade requirement")
	}
	if err := ensureSnapshotCanRemoveAlone(context.Background(), snapshotter, "mem"); err == nil {
		t.Fatal("mem remove error = nil, want referencing rootfs error")
	}
}

func TestSnapshotRemoveCascadeRemovesConchGroup(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"rootfs": {
			Name: "rootfs",
			Labels: map[string]string{
				common.SnapshotLabelMemSnapshot: "mem",
				common.SnapshotLabelVMSnapshot:  "vm",
			},
		},
		"mem": {Name: "mem"},
		"vm":  {Name: "vm"},
	})

	if err := removeSnapshotCascade(context.Background(), snapshotter, "mem"); err != nil {
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

func TestSnapshotRemoveCascadeDoesNotRemoveComponentsWhenRootfsFails(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"rootfs": {
			Name: "rootfs",
			Labels: map[string]string{
				common.SnapshotLabelMemSnapshot: "mem",
				common.SnapshotLabelVMSnapshot:  "vm",
			},
		},
		"mem": {Name: "mem"},
		"vm":  {Name: "vm"},
	})
	snapshotter.removeErrs = map[string]error{"rootfs": errdefs.ErrFailedPrecondition}

	if err := removeSnapshotCascade(context.Background(), snapshotter, "rootfs"); err == nil {
		t.Fatal("removeSnapshotCascade() error = nil, want rootfs remove error")
	}
	if len(snapshotter.removed) != 0 {
		t.Fatalf("removed = %#v, want none", snapshotter.removed)
	}
}

func TestSnapshotRemoveCascadePreservesGroupLabelsWhenComponentFails(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"rootfs": {
			Name: "rootfs",
			Labels: map[string]string{
				common.SnapshotLabelMemSnapshot: "mem",
				common.SnapshotLabelVMSnapshot:  "vm",
			},
		},
		"mem": {Name: "mem"},
		"vm":  {Name: "vm"},
	})
	snapshotter.removeErrs = map[string]error{"mem": errdefs.ErrFailedPrecondition}

	if err := removeSnapshotCascade(context.Background(), snapshotter, "rootfs"); err == nil {
		t.Fatal("removeSnapshotCascade() error = nil, want component remove error")
	}
	want := []string{"rootfs", "vm"}
	if len(snapshotter.removed) != len(want) {
		t.Fatalf("removed = %#v, want %#v", snapshotter.removed, want)
	}
	for i := range want {
		if snapshotter.removed[i] != want[i] {
			t.Fatalf("removed = %#v, want %#v", snapshotter.removed, want)
		}
	}
	memInfo, err := snapshotter.Stat(context.Background(), "mem")
	if err != nil {
		t.Fatalf("mem Stat() error = %v", err)
	}
	if memInfo.Labels[common.SnapshotLabelRootfsSnapshot] != "rootfs" ||
		memInfo.Labels[common.SnapshotLabelMemSnapshot] != "mem" ||
		memInfo.Labels[common.SnapshotLabelVMSnapshot] != "vm" {
		t.Fatalf("mem labels = %#v, want complete group labels", memInfo.Labels)
	}

	snapshotter.removeErrs = nil
	snapshotter.removed = nil
	if err := removeSnapshotCascade(context.Background(), snapshotter, "mem"); err != nil {
		t.Fatalf("recover removeSnapshotCascade() error = %v", err)
	}
	if len(snapshotter.removed) != 1 || snapshotter.removed[0] != "mem" {
		t.Fatalf("recover removed = %#v, want [mem]", snapshotter.removed)
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
