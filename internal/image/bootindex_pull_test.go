package image

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

type recordingResolver struct {
	resolvedName string
	target       ocispec.Descriptor
	fetchRef     string
}

func (r *recordingResolver) Resolve(context.Context, string) (string, ocispec.Descriptor, error) {
	return r.resolvedName, r.target, nil
}

func (r *recordingResolver) Fetcher(_ context.Context, ref string) (remotes.Fetcher, error) {
	r.fetchRef = ref
	return nil, nil
}

func (*recordingResolver) Pusher(context.Context, string) (remotes.Pusher, error) {
	return nil, nil
}

func TestTemporaryImageResolverSeparatesRemoteAndLocalNames(t *testing.T) {
	target := ocispec.Descriptor{Digest: digest.FromString("template")}
	underlying := &recordingResolver{
		resolvedName: "registry.example/conch/template:latest",
		target:       target,
	}
	resolver := &temporaryImageResolver{
		Resolver:      underlying,
		temporaryName: "localhost/conch/template-fetch:temporary",
	}

	name, gotTarget, err := resolver.Resolve(context.Background(), "registry.example/conch/template:latest")
	if err != nil {
		t.Fatal(err)
	}
	if name != resolver.temporaryName || gotTarget.Digest != target.Digest {
		t.Fatalf("Resolve() = %q, %#v", name, gotTarget)
	}
	if _, err := resolver.Fetcher(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if underlying.fetchRef != underlying.resolvedName {
		t.Fatalf("underlying Fetcher ref = %q, want %q", underlying.fetchRef, underlying.resolvedName)
	}
}

type cleanupImageStore struct {
	images.Store
	name        string
	target      ocispec.Descriptor
	namespace   string
	hasDeadline bool
}

func (s *cleanupImageStore) Delete(ctx context.Context, name string, opts ...images.DeleteOpt) error {
	s.name = name
	s.namespace, _ = namespaces.NamespaceRequired(ctx)
	_, s.hasDeadline = ctx.Deadline()
	var deleteOptions images.DeleteOptions
	for _, opt := range opts {
		if err := opt(ctx, &deleteOptions); err != nil {
			return err
		}
	}
	if deleteOptions.Target != nil {
		s.target = *deleteOptions.Target
	}
	return ctx.Err()
}

func TestRemoveTemporaryImageRecordUsesDetachedNamespacedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := ocispec.Descriptor{Digest: digest.FromString("temporary")}
	store := &cleanupImageStore{}

	if err := removeTemporaryImageRecord(ctx, store, "temporary", target); err != nil {
		t.Fatal(err)
	}
	if store.name != "temporary" || store.target.Digest != target.Digest || store.namespace != containerdclient.Namespace || !store.hasDeadline {
		t.Fatalf("cleanup call = %#v", store)
	}
}
