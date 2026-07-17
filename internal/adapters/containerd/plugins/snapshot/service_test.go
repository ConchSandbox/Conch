package snapshot

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
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
	if _, err := snapshotter.Stat(context.Background(), "other"); err != nil {
		t.Fatalf("unselected snapshot was removed: %v", err)
	}
}

func TestSnapshotRemoveNotFoundIsNoop(t *testing.T) {
	snapshotter := newFakeSnapshotter(nil)
	if err := removeSnapshotKey(context.Background(), snapshotter, "missing"); err != nil {
		t.Fatalf("removeSnapshotKey() error = %v", err)
	}
}

func TestSnapshotMetaPreservesContainerdMetadata(t *testing.T) {
	info := snapshots.Info{Name: "snap", Parent: "parent", Kind: snapshots.KindCommitted}
	got := snapshotMeta(info)
	if got.Key != "snap" || got.Parent != "parent" || got.Kind != "committed" {
		t.Fatalf("snapshotMeta() = %#v", got)
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
