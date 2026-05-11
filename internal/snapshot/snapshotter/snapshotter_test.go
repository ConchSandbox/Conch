package snapshotter

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	ctdnamespaces "github.com/containerd/containerd/v2/pkg/namespaces"
)

func TestContainerdSnapUsesDefaultNamespace(t *testing.T) {
	sn := &recordingSnapshotter{}
	store := &fakeNamespaceStore{items: []string{"team-a"}}
	s, err := NewContainerdSnap(sn, store, "team-a")
	if err != nil {
		t.Fatalf("NewContainerdSnap: %v", err)
	}

	if _, err := s.Prepare(context.Background(), "", "active", "", nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if sn.lastNamespace != "team-a" {
		t.Fatalf("namespace = %q, want team-a", sn.lastNamespace)
	}
}

func TestContainerdSnapUsesExplicitNamespaceAndListsNamespaces(t *testing.T) {
	sn := &recordingSnapshotter{}
	store := &fakeNamespaceStore{items: []string{"team-a", "team-b"}}
	s, err := NewContainerdSnap(sn, store, "default")
	if err != nil {
		t.Fatalf("NewContainerdSnap: %v", err)
	}

	if _, err := s.Stat(context.Background(), "team-b", "committed"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if sn.lastNamespace != "team-b" {
		t.Fatalf("namespace = %q, want team-b", sn.lastNamespace)
	}

	items, err := s.ListNamespaces(context.Background())
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(items) != 2 || items[0] != "team-a" || items[1] != "team-b" {
		t.Fatalf("namespaces = %v, want [team-a team-b]", items)
	}
}

func TestNewContainerdSnapValidatesDependencies(t *testing.T) {
	store := &fakeNamespaceStore{}
	if _, err := NewContainerdSnap(nil, store, "default"); err == nil {
		t.Fatal("NewContainerdSnap with nil snapshotter succeeded")
	}
	if _, err := NewContainerdSnap(&recordingSnapshotter{}, nil, "default"); err == nil {
		t.Fatal("NewContainerdSnap with nil namespace store succeeded")
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

type fakeNamespaceStore struct {
	items []string
}

func (f *fakeNamespaceStore) Create(ctx context.Context, namespace string, labels map[string]string) error {
	return nil
}

func (f *fakeNamespaceStore) Labels(ctx context.Context, namespace string) (map[string]string, error) {
	return nil, nil
}

func (f *fakeNamespaceStore) SetLabel(ctx context.Context, namespace, key, value string) error {
	return nil
}

func (f *fakeNamespaceStore) List(ctx context.Context) ([]string, error) {
	return f.items, nil
}

func (f *fakeNamespaceStore) Delete(ctx context.Context, namespace string, opts ...ctdnamespaces.DeleteOpts) error {
	return nil
}
