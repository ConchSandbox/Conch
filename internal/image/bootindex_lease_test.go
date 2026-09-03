package image

import (
	"context"
	"path/filepath"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

func TestBootIndexOperationsRetainContentForTheirLifetime(t *testing.T) {
	ctx := context.Background()
	leaseManager := &recordingLeaseManager{}
	contentStore, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatalf("create content store: %v", err)
	}
	client, err := containerd.New("", containerd.WithServices(
		containerd.WithContentStore(contentStore),
		containerd.WithLeasesService(leaseManager),
	))
	if err != nil {
		t.Fatalf("create containerd client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	conchClient := &containerdclient.Client{Client: client}
	digest := digest.FromString("missing-boot-index").String()

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "push",
			run: func() error {
				return PushBootIndex(ctx, conchClient, PushBootIndexOptions{
					BootIndexDigest: digest,
					RemoteReference: "registry.example/conch/template:latest",
				})
			},
		},
		{
			name: "unpack",
			run: func() error {
				return UnpackBootIndex(ctx, conchClient, digest)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			leaseManager.reset()
			if err := tc.run(); err == nil {
				t.Fatal("operation unexpectedly succeeded with missing Boot Index content")
			}
			want := leases.Resource{Type: "content", ID: digest}
			if leaseManager.added != want {
				t.Fatalf("retained resource = %#v, want %#v", leaseManager.added, want)
			}
			if !leaseManager.deleted {
				t.Fatal("operation lease was not deleted")
			}
		})
	}
}

type recordingLeaseManager struct {
	leases.Manager
	lease   leases.Lease
	added   leases.Resource
	deleted bool
}

func (m *recordingLeaseManager) reset() {
	m.lease = leases.Lease{}
	m.added = leases.Resource{}
	m.deleted = false
}

func (m *recordingLeaseManager) Create(_ context.Context, opts ...leases.Opt) (leases.Lease, error) {
	m.lease = leases.Lease{ID: "operation-lease"}
	for _, opt := range opts {
		if err := opt(&m.lease); err != nil {
			return leases.Lease{}, err
		}
	}
	return m.lease, nil
}

func (m *recordingLeaseManager) Delete(_ context.Context, lease leases.Lease, _ ...leases.DeleteOpt) error {
	m.deleted = true
	return nil
}

func (m *recordingLeaseManager) AddResource(_ context.Context, lease leases.Lease, resource leases.Resource) error {
	m.added = resource
	return nil
}
