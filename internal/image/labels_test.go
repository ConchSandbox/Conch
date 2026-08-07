package image

import (
	"context"
	"reflect"
	"testing"

	"github.com/containerd/containerd/v2/core/images"
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
	if got := DetectImageKind(context.Background(), nil, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest}); got != ImageKindOCIImage {
		t.Fatalf("DetectImageKind() = %q, want %q", got, ImageKindOCIImage)
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
