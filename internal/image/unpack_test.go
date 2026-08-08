package image

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestGetKindDefaultsToUnknown(t *testing.T) {
	got := getKind(ocispec.Descriptor{})
	if got != KindUnknown {
		t.Fatalf("kind: got %q want %q", got, KindUnknown)
	}
}

func TestValidateNativeComponentManifestRejectsEmptyLayers(t *testing.T) {
	ctx := context.Background()
	store, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	client, err := containerd.New("", containerd.WithServices(containerd.WithContentStore(store)))
	if err != nil {
		t.Fatalf("new containerd client: %v", err)
	}

	manifestDesc, err := writeBlobJSONToContent(ctx, store, ocispec.Manifest{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    digest.FromString("empty-component-config"),
			Size:      1,
		},
	}, ocispec.MediaTypeImageManifest)
	if err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	for _, kind := range []string{KindRootfs, KindSandbox, KindMemSnapshot} {
		t.Run(kind, func(t *testing.T) {
			err := validateNativeComponentManifest(ctx, client, kind, manifestDesc)
			if err == nil || !strings.Contains(err.Error(), "no layers") {
				t.Fatalf("validateNativeComponentManifest() error = %v, want no layers", err)
			}
		})
	}
}

func TestValidateBootIndexManifestKindsRejectsDuplicateAndUnknownKinds(t *testing.T) {
	descriptor := func(kind, payload string) ocispec.Descriptor {
		return ocispec.Descriptor{
			MediaType:   ocispec.MediaTypeImageManifest,
			Digest:      digest.FromString(payload),
			Size:        1,
			Annotations: map[string]string{"io.conch.kind": kind},
		}
	}

	_, err := validateBootIndexManifestKinds([]ocispec.Descriptor{
		descriptor(KindRootfs, "rootfs-a"),
		descriptor(KindRootfs, "rootfs-b"),
		descriptor(KindSandbox, "sandbox"),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate kind error = %v", err)
	}

	_, err = validateBootIndexManifestKinds([]ocispec.Descriptor{
		descriptor(KindRootfs, "rootfs"),
		descriptor(KindUnknown, "unknown"),
		descriptor(KindSandbox, "sandbox"),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown kind error = %v", err)
	}
}
