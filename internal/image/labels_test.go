package image

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/images"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type recordingImageStore struct {
	updated    images.Image
	fieldpaths []string
}

func (s *recordingImageStore) Get(context.Context, string) (images.Image, error) {
	return images.Image{}, nil
}

func (s *recordingImageStore) List(context.Context, ...string) ([]images.Image, error) {
	return nil, nil
}

func (s *recordingImageStore) Create(context.Context, images.Image) (images.Image, error) {
	return images.Image{}, nil
}

func (s *recordingImageStore) Update(_ context.Context, image images.Image, fieldpaths ...string) (images.Image, error) {
	s.updated = image
	s.fieldpaths = append([]string(nil), fieldpaths...)
	return image, nil
}

func (s *recordingImageStore) Delete(context.Context, string, ...images.DeleteOpt) error {
	return nil
}

func TestComponentImageKind(t *testing.T) {
	tests := map[string]string{
		KindRootfs:      ImageKindBootComponentRootfs,
		KindSandbox:     ImageKindBootComponentSandbox,
		KindMemSnapshot: ImageKindBootComponentMemory,
		KindUnknown:     ImageKindOCIImage,
	}
	for componentKind, want := range tests {
		if got := componentImageKind(componentKind); got != want {
			t.Fatalf("componentImageKind(%q) = %q, want %q", componentKind, got, want)
		}
	}
}

func TestDetectImageKindDefaultsNonIndexToOCI(t *testing.T) {
	got, err := DetectImageKind(context.Background(), nil, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest})
	if err != nil {
		t.Fatalf("DetectImageKind() error = %v", err)
	}
	if got != ImageKindOCIImage {
		t.Fatalf("DetectImageKind() = %q, want %q", got, ImageKindOCIImage)
	}
}

func TestDetectImageKindUsesOnlyTopLevelIndex(t *testing.T) {
	descriptor := func(kind, seed string) ocispec.Descriptor {
		desc := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.FromString(seed),
			Size:      1,
		}
		if kind != "" {
			desc.Annotations = map[string]string{"io.conch.kind": kind}
		}
		return desc
	}
	resumeMemory := descriptor(KindMemSnapshot, "resume-memory")
	resumeMemory.Annotations[AnnotationVMM] = "cloud-hypervisor"
	resumeMemory.Annotations[AnnotationMemorySizeMB] = "512"

	tests := []struct {
		name        string
		manifests   []ocispec.Descriptor
		annotations map[string]string
		want        string
	}{
		{
			name: "ordinary OCI index",
			manifests: []ocispec.Descriptor{
				descriptor("", "ordinary"),
			},
			want: ImageKindOCIImage,
		},
		{
			name: "cold Boot Index",
			manifests: []ocispec.Descriptor{
				descriptor(KindRootfs, "cold-rootfs"),
				descriptor(KindSandbox, "cold-sandbox"),
			},
			want: ImageKindBootIndexCold,
		},
		{
			name: "resume Boot Index",
			manifests: []ocispec.Descriptor{
				descriptor(KindRootfs, "resume-rootfs"),
				resumeMemory,
				descriptor(KindSandbox, "resume-sandbox"),
			},
			annotations: map[string]string{
				AnnotationVMM:          "cloud-hypervisor",
				AnnotationMemorySizeMB: "512",
			},
			want: ImageKindBootIndexResume,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			index := ocispec.Index{
				Versioned:   ispec.Versioned{SchemaVersion: 2},
				MediaType:   ocispec.MediaTypeImageIndex,
				Manifests:   tt.manifests,
				Annotations: tt.annotations,
			}
			indexDesc, err := writeBlobJSONToContent(ctx, store, index, ocispec.MediaTypeImageIndex)
			if err != nil {
				t.Fatalf("write index: %v", err)
			}

			got, err := DetectImageKind(ctx, store, indexDesc)
			if err != nil {
				t.Fatalf("DetectImageKind() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("DetectImageKind() = %q, want %q", got, tt.want)
			}
			for _, child := range tt.manifests {
				if _, err := store.Info(ctx, child.Digest); err == nil {
					t.Fatalf("child %s unexpectedly exists in content store", child.Digest)
				}
			}
		})
	}
}

func TestDetectImageKindRejectsMalformedConchIndex(t *testing.T) {
	ctx := context.Background()
	store, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	index := ocispec.Index{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.FromString("rootfs-only"),
			Size:      1,
			Annotations: map[string]string{
				"io.conch.kind": KindRootfs,
			},
		}},
	}
	indexDesc, err := writeBlobJSONToContent(ctx, store, index, ocispec.MediaTypeImageIndex)
	if err != nil {
		t.Fatalf("write index: %v", err)
	}

	_, err = DetectImageKind(ctx, store, indexDesc)
	if err == nil || !strings.Contains(err.Error(), "missing required kind \"sandbox\"") {
		t.Fatalf("DetectImageKind() error = %v, want missing sandbox", err)
	}
}

func TestValidatePullKind(t *testing.T) {
	tests := []struct {
		name          string
		kind          string
		bootIndexOnly bool
		wantReject    bool
	}{
		{
			name:          "image pull accepts OCI image",
			kind:          ImageKindOCIImage,
			bootIndexOnly: false,
		},
		{
			name:          "image pull rejects cold Boot Index",
			kind:          ImageKindBootIndexCold,
			bootIndexOnly: false,
			wantReject:    true,
		},
		{
			name:          "image pull rejects resume Boot Index",
			kind:          ImageKindBootIndexResume,
			bootIndexOnly: false,
			wantReject:    true,
		},
		{
			name:          "template pull rejects OCI image",
			kind:          ImageKindOCIImage,
			bootIndexOnly: true,
			wantReject:    true,
		},
		{
			name:          "template pull accepts cold Boot Index",
			kind:          ImageKindBootIndexCold,
			bootIndexOnly: true,
		},
		{
			name:          "template pull accepts resume Boot Index",
			kind:          ImageKindBootIndexResume,
			bootIndexOnly: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePullKind("registry.example.invalid/conch/test:latest", tt.kind, tt.bootIndexOnly)
			if tt.wantReject {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("validatePullKind() error = %v, want ErrInvalidRequest", err)
				}
			} else if err != nil {
				t.Fatalf("validatePullKind() error = %v", err)
			}
		})
	}
}

func TestSetImageKindLabelUpdatesOnlyCanonicalLabel(t *testing.T) {
	store := &recordingImageStore{}
	if err := SetImageKindLabel(context.Background(), store, "example:latest", ImageKindBootIndexCold); err != nil {
		t.Fatalf("SetImageKindLabel() error = %v", err)
	}
	if store.updated.Name != "example:latest" || store.updated.Labels[ImageKindLabel] != ImageKindBootIndexCold {
		t.Fatalf("updated image = %#v", store.updated)
	}
	wantFields := []string{"labels." + ImageKindLabel}
	if !reflect.DeepEqual(store.fieldpaths, wantFields) {
		t.Fatalf("fieldpaths = %#v, want %#v", store.fieldpaths, wantFields)
	}
}
