package snapshotter

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	ctdnamespaces "github.com/containerd/containerd/v2/pkg/namespaces"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

func TestContainerdSnapUsesFixedConchNamespace(t *testing.T) {
	sn := &recordingSnapshotter{}
	s, err := NewContainerdSnap(sn)
	if err != nil {
		t.Fatalf("NewContainerdSnap: %v", err)
	}

	if _, err := s.Prepare(context.Background(), "active", "", nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if sn.lastNamespace != containerdclient.Namespace {
		t.Fatalf("namespace = %q, want %s", sn.lastNamespace, containerdclient.Namespace)
	}
}

func TestNewContainerdSnapValidatesDependencies(t *testing.T) {
	if _, err := NewContainerdSnap(nil); err == nil {
		t.Fatal("NewContainerdSnap with nil snapshotter succeeded")
	}
}

type recordingSnapshotter struct {
	lastNamespace string
}

func (r *recordingSnapshotter) record(ctx context.Context) error {
	ns, ok := ctdnamespaces.Namespace(ctx)
	if !ok {
		return errors.New("namespace missing")
	}
	r.lastNamespace = ns
	return nil
}

func (r *recordingSnapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	return snapshots.Info{Name: key}, r.record(ctx)
}

func (r *recordingSnapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	return info, r.record(ctx)
}

func (r *recordingSnapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	return snapshots.Usage{}, r.record(ctx)
}

func (r *recordingSnapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	return nil, r.record(ctx)
}

func (r *recordingSnapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, r.record(ctx)
}

func (r *recordingSnapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, r.record(ctx)
}

func (r *recordingSnapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return r.record(ctx)
}

func (r *recordingSnapshotter) Remove(ctx context.Context, key string) error {
	return r.record(ctx)
}

func (r *recordingSnapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	if err := r.record(ctx); err != nil {
		return err
	}
	return nil
}

func (r *recordingSnapshotter) Close() error {
	return nil
}
