package snapshotter

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

// Snapshotter defines the snapshot management operation interface
type Snapshotter interface {
	Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error)
	View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error)
	Mounts(ctx context.Context, key string) ([]mount.Mount, error)
	Commit(ctx context.Context, key, snapshotID string, opts ...snapshots.Opt) error
	Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error)
	Remove(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (snapshots.Info, error)
	List(ctx context.Context, result map[string]*snapshots.Info, filters ...string) error
	Close() error
}

// ContainerdSnap is the containerd implementation of Snapshotter
type ContainerdSnap struct {
	snapshotter snapshots.Snapshotter
}

// NewContainerdSnap creates a containerd snapshotter instance
func NewContainerdSnap(snapshotter snapshots.Snapshotter) (Snapshotter, error) {
	if snapshotter == nil {
		return nil, fmt.Errorf("containerd snapshotter is nil")
	}
	return &ContainerdSnap{snapshotter: snapshotter}, nil
}

// Close releases resources
func (c *ContainerdSnap) Close() error {
	return c.snapshotter.Close()
}

// getSnapshotterAndContext binds all snapshot operations to Conch's fixed
// containerd namespace.
func (c *ContainerdSnap) getSnapshotterAndContext(ctx context.Context) (snapshots.Snapshotter, context.Context) {
	return c.snapshotter, containerdclient.NewNamespaceContext(ctx)
}

// Prepare creates a writable snapshot
func (c *ContainerdSnap) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	sn, nsCtx := c.getSnapshotterAndContext(ctx)
	return sn.Prepare(nsCtx, key, parent, opts...)
}

// View creates a read-only snapshot view
func (c *ContainerdSnap) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	sn, nsCtx := c.getSnapshotterAndContext(ctx)
	return sn.View(nsCtx, key, parent, opts...)
}

func (c *ContainerdSnap) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	sn, nsCtx := c.getSnapshotterAndContext(ctx)
	return sn.Mounts(nsCtx, key)
}

// Commit commits an active snapshot to a persistent snapshot
func (c *ContainerdSnap) Commit(ctx context.Context, key, snapshotID string, opts ...snapshots.Opt) error {
	sn, nsCtx := c.getSnapshotterAndContext(ctx)
	return sn.Commit(nsCtx, snapshotID, key, opts...)
}

func (c *ContainerdSnap) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	sn, nsCtx := c.getSnapshotterAndContext(ctx)
	return sn.Update(nsCtx, info, fieldpaths...)
}

// Remove removes a snapshot
func (c *ContainerdSnap) Remove(ctx context.Context, key string) error {
	sn, nsCtx := c.getSnapshotterAndContext(ctx)
	return sn.Remove(nsCtx, key)
}

// Stat gets snapshot information
func (c *ContainerdSnap) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	sn, nsCtx := c.getSnapshotterAndContext(ctx)
	return sn.Stat(nsCtx, key)
}

// List lists snapshots in the fixed Conch namespace.
func (c *ContainerdSnap) List(ctx context.Context, result map[string]*snapshots.Info, filters ...string) error {
	walk := func(ctx context.Context, info snapshots.Info) error {
		result[info.Name] = &info
		return nil
	}

	sn, nsCtx := c.getSnapshotterAndContext(ctx)
	if err := sn.Walk(nsCtx, walk, filters...); err != nil {
		return fmt.Errorf("walk snapshots: %w", err)
	}

	return nil
}
