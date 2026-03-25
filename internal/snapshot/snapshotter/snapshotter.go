package snapshotter

import (
	"context"
	"fmt"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"

	"github.com/openeuler/Conch/internal/daemon"
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
	client *daemon.Client
}

// NewContainerdSnap creates a containerd snapshotter instance
func NewContainerdSnap(client *daemon.Client) (Snapshotter, error) {
	if client == nil {
		return nil, fmt.Errorf("containerd client is nil")
	}
	return &ContainerdSnap{client: client}, nil
}

// Close releases resources
func (c *ContainerdSnap) Close() error {
	return nil
}

// getSnapshotterAndContext gets the snapshotter instance and namespace context
func (c *ContainerdSnap) getSnapshotterAndContext(ctx context.Context, namespace string) (snapshots.Snapshotter, context.Context, error) {
	nsCtx, err := c.client.WithNamespace(ctx, namespace)
	if err != nil {
		return nil, nil, fmt.Errorf("create namespace context: %w", err)
	}
	sn := c.client.SnapshotService(containerd.DefaultSnapshotter)
	return sn, nsCtx, nil
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
	nss := c.client.NamespaceService()
	items, err := nss.List(ctx)
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
