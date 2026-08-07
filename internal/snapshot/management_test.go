package snapshot

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
)

func TestSnapshotRemoveRemovesExactlyOneKey(t *testing.T) {
	snapshotter := newFakeSnapshotter(map[string]snapshots.Info{
		"selected": {Name: "selected"},
		"other":    {Name: "other"},
	})

	if err := removeSnapshotKey(context.Background(), snapshotter, "selected"); err != nil {
		t.Fatalf("removeSnapshotKey() error = %v", err)
	}
	if len(snapshotter.removed) != 1 || snapshotter.removed[0] != "selected" {
		t.Fatalf("removed = %#v, want [selected]", snapshotter.removed)
	}
	if _, ok := snapshotter.infos["other"]; !ok {
		t.Fatal("unselected snapshot was removed")
	}
}

func TestSnapshotRemoveNotFoundIsNoop(t *testing.T) {
	snapshotter := newFakeSnapshotter(nil)
	if err := removeSnapshotKey(context.Background(), snapshotter, "missing"); err != nil {
		t.Fatalf("removeSnapshotKey() error = %v", err)
	}
}

func TestSnapshotRecordPreservesContainerdMetadata(t *testing.T) {
	info := snapshots.Info{Name: "snap", Parent: "parent", Kind: snapshots.KindCommitted}
	got := snapshotRecord(info)
	if got.Key != "snap" || got.Parent != "parent" || got.Kind != "committed" {
		t.Fatalf("snapshotRecord() = %#v", got)
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
