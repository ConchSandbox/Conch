package snapshot

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

func TestCommitMemSnapshotLabelsComponentKind(t *testing.T) {
	snapshotter := &recordingOpsSnapshotter{}
	ops := &snapshotOps{server: &Server{snt: snapshotter}}

	if err := ops.commitMemSnapshot(context.Background(), "default", "mem-active", "mem-committed", "rootfs-id"); err != nil {
		t.Fatalf("commitMemSnapshot() error = %v", err)
	}
	if snapshotter.committedInfo.Labels[common.SnapshotLabelGroupID] != "rootfs-id" {
		t.Fatalf("group id label = %q, want rootfs-id", snapshotter.committedInfo.Labels[common.SnapshotLabelGroupID])
	}
	if snapshotter.committedInfo.Labels[common.SnapshotLabelComponentKind] != common.SnapshotComponentKindMem {
		t.Fatalf("component kind = %q, want %q", snapshotter.committedInfo.Labels[common.SnapshotLabelComponentKind], common.SnapshotComponentKindMem)
	}
}

type recordingOpsSnapshotter struct {
	committedInfo snapshots.Info
}

func (r *recordingOpsSnapshotter) Prepare(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (r *recordingOpsSnapshotter) View(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (r *recordingOpsSnapshotter) Mounts(context.Context, string, string) ([]mount.Mount, error) {
	return nil, nil
}

func (r *recordingOpsSnapshotter) Commit(_ context.Context, _, _, snapshotID string, opts ...snapshots.Opt) error {
	info := snapshots.Info{Name: snapshotID}
	for _, opt := range opts {
		if err := opt(&info); err != nil {
			return err
		}
	}
	r.committedInfo = info
	return nil
}

func (r *recordingOpsSnapshotter) Update(context.Context, string, snapshots.Info, ...string) (snapshots.Info, error) {
	return snapshots.Info{}, nil
}

func (r *recordingOpsSnapshotter) Remove(context.Context, string, string) error {
	return nil
}

func (r *recordingOpsSnapshotter) Stat(context.Context, string, string) (snapshots.Info, error) {
	return snapshots.Info{}, nil
}

func (r *recordingOpsSnapshotter) List(context.Context, string, map[string]*snapshots.Info, ...string) error {
	return nil
}

func (r *recordingOpsSnapshotter) ListNamespaces(context.Context) ([]string, error) {
	return nil, nil
}

func (r *recordingOpsSnapshotter) Close() error {
	return nil
}
