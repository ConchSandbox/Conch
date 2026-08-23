package image

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/images"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRemoveFetchedImageRecordDetachesCleanupFromRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &cleanupImageStore{}

	if err := RemoveFetchedImageRecord(
		ctx, store, "registry.example.invalid/conch/template:latest", ocispec.Descriptor{},
	); err != nil {
		t.Fatalf("RemoveFetchedImageRecord() error = %v", err)
	}
	if !store.hasDeadline {
		t.Fatal("Delete() context has no cleanup deadline")
	}
}

type cleanupImageStore struct {
	images.Store
	hasDeadline bool
}

func (s *cleanupImageStore) Delete(ctx context.Context, _ string, _ ...images.DeleteOpt) error {
	_, s.hasDeadline = ctx.Deadline()
	return ctx.Err()
}

func TestImageRepoDigests(t *testing.T) {
	tests := []struct {
		name   string
		ref    string
		digest string
		want   []string
	}{
		{
			name:   "tagged image",
			ref:    "registry.example.invalid/conch/demo:latest",
			digest: "sha256:demo",
			want:   []string{"registry.example.invalid/conch/demo@sha256:demo"},
		},
		{
			name:   "repo digest image",
			ref:    "registry.example.invalid/conch/demo@sha256:old",
			digest: "sha256:demo",
			want:   []string{"registry.example.invalid/conch/demo@sha256:demo"},
		},
		{
			name:   "digest only",
			ref:    "sha256:demo",
			digest: "sha256:demo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := imageRepoDigests(tt.ref, tt.digest)
			if len(got) != len(tt.want) {
				t.Fatalf("imageRepoDigests() = %#v, want %#v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("imageRepoDigests()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
