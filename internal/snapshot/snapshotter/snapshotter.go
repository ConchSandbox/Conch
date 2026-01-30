package snapshotter

import (
	"context"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
)

const (
	SnapshotLabel            = "conch/snapshotter/snapshot"
	SnapshotLabelRootfs      = "conch/snapshotter/snapshot-rootfs"
	SnapshotLabelSnapshotDir = "conch/snapshotter/snapshot-dir"
	SnapshotLabelMemSize     = "conch/snapshotter/snapshot-memsize"

	// binding labels between mem and rootfs snapshots
	SnapshotLabelMemSnapshot    = "conch/snapshotter/mem-snapshot"
	SnapshotLabelRootfsSnapshot = "conch/snapshotter/rootfs-snapshot"
)

type Snapshotter interface {
	Prepare(ctx context.Context, ns, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error)
	View(ctx context.Context, ns, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error)
	Commit(ctx context.Context, ns, key, name string, opts ...snapshots.Opt) error
	Remove(ctx context.Context, ns, key string) error
	Stat(ctx context.Context, ns, key string) (snapshots.Info, error)
	List(ctx context.Context, namespace string, result map[string]*snapshots.Info, filters ...string) error
	ListNamespaces(ctx context.Context) ([]string, error)
	Close() error
}
