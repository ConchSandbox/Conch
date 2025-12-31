package snapshotter

import (
	"conch/core/snapshot/common"
	"context"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
)

type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	sn     snapshots.Snapshotter
}

type ContainerdSnap struct {
	sessions map[string]*session
	client   *containerd.Client
	mu       sync.RWMutex
}

func NewContainerdSnap() (Snapshotter, error) {
	cs := &ContainerdSnap{
		sessions: make(map[string]*session),
	}
	client, err := initContainerdClient(common.CONTAINERD_SOCK)
	if err != nil {
		return nil, err
	}
	cs.client = client

	return cs, nil
}

func (c *ContainerdSnap) Close() error {
	var result error
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.sessions {
		v.cancel()
		delete(c.sessions, k)
	}
	c.client.Close()
	return result
}

func (c *ContainerdSnap) addSnapshotter(ctx context.Context, namespace string) (snapshots.Snapshotter, error) {

	var cancel context.CancelFunc
	ns := namespace
	if namespace == "" {
		ns = c.client.DefaultNamespace()
	}
	ctx, cancel = context.WithCancel(namespaces.WithNamespace(ctx, ns))

	sn := c.client.SnapshotService(containerd.DefaultSnapshotter)
	c.sessions[ns] = &session{ctx: ctx, cancel: cancel, sn: sn}
	return sn, nil
}

func (c *ContainerdSnap) Prepare(ctx context.Context, ns, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, ok := c.sessions[ns]
	if !ok {
		_, err := c.addSnapshotter(ctx, ns)
		if err != nil {
			return nil, fmt.Errorf("add snapshotter failed: %v", err)
		}
		session, ok = c.sessions[ns]
		if !ok {
			return nil, fmt.Errorf("fatal: add snapshotter for %s failed", ns)
		}
	}
	return session.sn.Prepare(session.ctx, key, parent, opts...)
}

func (c *ContainerdSnap) Commit(ctx context.Context, ns, key, name string, opts ...snapshots.Opt) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	session, ok := c.sessions[ns]
	if !ok {
		return fmt.Errorf("snapshotter of %s is not init", ns)
	}
	return session.sn.Commit(session.ctx, name, key, opts...)
}

func (c *ContainerdSnap) Remove(ctx context.Context, ns, key string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	session, ok := c.sessions[ns]
	if !ok {
		return fmt.Errorf("snapshotter of %s is not init", ns)
	}
	return session.sn.Remove(session.ctx, key)
}

func (c *ContainerdSnap) Stat(ctx context.Context, ns, key string) (snapshots.Info, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	session, ok := c.sessions[ns]
	if !ok {
		return snapshots.Info{}, fmt.Errorf("snapshotter of %s is not init", ns)
	}
	return session.sn.Stat(session.ctx, key)
}

func (c *ContainerdSnap) ListNamespaces(ctx context.Context) ([]string, error) {
	nss := c.client.NamespaceService()
	items, err := nss.List(ctx)
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (c *ContainerdSnap) List(ctx context.Context, namespace string, result map[string]*snapshots.Info, filters ...string) error {
	walk := func(ctx context.Context, info snapshots.Info) error {
		result[info.Name] = &info
		return nil
	}

	c.mu.RLock()
	session, ok := c.sessions[namespace]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, err := c.addSnapshotter(ctx, namespace)
		if err != nil {
			return err
		}
		if session, ok = c.sessions[namespace]; !ok {
			return fmt.Errorf("snapshotter of %s is not init", namespace)
		}
	}
	if err := session.sn.Walk(session.ctx, walk, filters...); err != nil {
		fmt.Printf("walk err: %v", err)
		return err
	}

	return nil
}

func checkSocket(s string) error {
	// set AT_EACCESS
	return unix.Faccessat(-1, s, unix.R_OK|unix.W_OK, unix.AT_EACCESS)
}

func initContainerdClient(address string, opts ...containerd.ClientOpt) (*containerd.Client, error) {
	address = strings.TrimPrefix(address, "unix://")
	if err := checkSocket(address); err != nil {
		err = fmt.Errorf("access containerd socket %q, err: %v", address, err)
		return nil, err
	}
	client, err := containerd.New(address, opts...)
	if err != nil {
		return nil, err
	}
	return client, nil
}
