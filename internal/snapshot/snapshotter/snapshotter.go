package snapshotter

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	ctdnamespaces "github.com/containerd/containerd/v2/pkg/namespaces"
)

// Snapshotter defines the snapshot management operation interface
type Snapshotter interface {
	Prepare(ctx context.Context, namespace, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error)
	View(ctx context.Context, namespace, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error)
	Commit(ctx context.Context, namespace, key, snapshotID string, opts ...snapshots.Opt) error
	Remove(ctx context.Context, namespace, key string) error
	Stat(ctx context.Context, namespace, key string) (snapshots.Info, error)
	List(ctx context.Context, namespace string, result map[string]*snapshots.Info, filters ...string) error
	ListNamespaces(ctx context.Context) ([]string, error)
	Close() error
}

// ContainerdSnap is the containerd implementation of Snapshotter
type ContainerdSnap struct {
	snapshotter      snapshots.Snapshotter
	namespaceStore   ctdnamespaces.Store
	defaultNamespace string
}

// NewContainerdSnap creates a containerd snapshotter instance
func NewContainerdSnap(snapshotter snapshots.Snapshotter, namespaceStore ctdnamespaces.Store, defaultNamespace string) (Snapshotter, error) {
	if snapshotter == nil {
		return nil, fmt.Errorf("containerd snapshotter is nil")
	}
	if namespaceStore == nil {
		return nil, fmt.Errorf("containerd namespace store is nil")
	}
	if defaultNamespace == "" {
		defaultNamespace = "default"
	}
	return &ContainerdSnap{
		snapshotter:      snapshotter,
		namespaceStore:   namespaceStore,
		defaultNamespace: defaultNamespace,
	}, nil
}

// Close releases resources
func (c *ContainerdSnap) Close() error {
	return c.snapshotter.Close()
}

// getSnapshotterAndContext gets the snapshotter instance and namespace context
func (c *ContainerdSnap) getSnapshotterAndContext(ctx context.Context, namespace string) (snapshots.Snapshotter, context.Context, error) {
	ns := namespace
	if ns == "" {
		ns = c.defaultNamespace
	}
	if ns == "" {
		ns = "default"
	}
	return c.snapshotter, ctdnamespaces.WithNamespace(ctx, ns), nil
}

// Prepare creates a writable snapshot
func (c *ContainerdSnap) Prepare(ctx context.Context, namespace, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	sn, nsCtx, err := c.getSnapshotterAndContext(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return sn.Prepare(nsCtx, key, parent, opts...)
}

// View creates a read-only snapshot view
func (c *ContainerdSnap) View(ctx context.Context, namespace, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	sn, nsCtx, err := c.getSnapshotterAndContext(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return sn.View(nsCtx, key, parent, opts...)
}

// Commit commits an active snapshot to a persistent snapshot
func (c *ContainerdSnap) Commit(ctx context.Context, namespace, key, snapshotID string, opts ...snapshots.Opt) error {
	sn, nsCtx, err := c.getSnapshotterAndContext(ctx, namespace)
	if err != nil {
		return err
	}
	return sn.Commit(nsCtx, snapshotID, key, opts...)
}

// Remove removes a snapshot
func (c *ContainerdSnap) Remove(ctx context.Context, namespace, key string) error {
	sn, nsCtx, err := c.getSnapshotterAndContext(ctx, namespace)
	if err != nil {
		return err
	}
	return sn.Remove(nsCtx, key)
}

// Stat gets snapshot information
func (c *ContainerdSnap) Stat(ctx context.Context, namespace, key string) (snapshots.Info, error) {
	sn, nsCtx, err := c.getSnapshotterAndContext(ctx, namespace)
	if err != nil {
		return snapshots.Info{}, err
	}
	return sn.Stat(nsCtx, key)
}

// ListNamespaces lists all namespaces
func (c *ContainerdSnap) ListNamespaces(ctx context.Context) ([]string, error) {
	items, err := c.namespaceStore.List(ctx)
	if err != nil {
		return nil, err
	}
	return items, nil
}

// List lists all snapshots in the specified namespace
func (c *ContainerdSnap) List(ctx context.Context, namespace string, result map[string]*snapshots.Info, filters ...string) error {
	walk := func(ctx context.Context, info snapshots.Info) error {
		result[info.Name] = &info
		return nil
	}

	sn, nsCtx, err := c.getSnapshotterAndContext(ctx, namespace)
	if err != nil {
		return err
	}
	if err := sn.Walk(nsCtx, walk, filters...); err != nil {
		return fmt.Errorf("walk snapshots: %w", err)
	}

	return nil
}
