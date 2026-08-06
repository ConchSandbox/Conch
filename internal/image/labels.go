package image

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// ImageKindLabel classifies a local containerd image record. Unlike OCI
	// descriptor annotations, image labels are local metadata and are not
	// propagated through registries.
	ImageKindLabel = "io.conch.image.kind"

	ImageKindOCIImage             = "oci-image"
	ImageKindBootIndexCold        = "boot-index-cold"
	ImageKindBootIndexResume      = "boot-index-resume"
	ImageKindBootComponentRootfs  = "boot-component-rootfs"
	ImageKindBootComponentSandbox = "boot-component-sandbox"
	ImageKindBootComponentMemory  = "boot-component-memory"
)

// DetectImageKind classifies newly ingested content before the result is
// persisted as ImageKindLabel on its image record.
func DetectImageKind(ctx context.Context, store content.Store, target ocispec.Descriptor) string {
	if target.MediaType != ocispec.MediaTypeImageIndex {
		return ImageKindOCIImage
	}
	info, err := InspectBootIndexContent(ctx, store, target)
	if err != nil {
		return ImageKindOCIImage
	}
	if info.Resume {
		return ImageKindBootIndexResume
	}
	return ImageKindBootIndexCold
}

// SetImageKindLabel updates only the Conch classification label and preserves
// all other image-record labels.
func SetImageKindLabel(ctx context.Context, store images.Store, imageName, kind string) error {
	if store == nil {
		return fmt.Errorf("image store is required")
	}
	imageName = strings.TrimSpace(imageName)
	if imageName == "" {
		return fmt.Errorf("image name is required")
	}
	if !validImageKind(kind) {
		return fmt.Errorf("invalid image kind %q", kind)
	}
	_, err := store.Update(ctx, images.Image{
		Name: imageName,
		Labels: map[string]string{
			ImageKindLabel: kind,
		},
	}, "labels."+ImageKindLabel)
	if err != nil {
		return fmt.Errorf("label image %s as %s: %w", imageName, kind, err)
	}
	return nil
}

func componentImageKind(componentKind string) string {
	switch componentKind {
	case KindRootfs:
		return ImageKindBootComponentRootfs
	case KindSandbox:
		return ImageKindBootComponentSandbox
	case KindMemSnapshot:
		return ImageKindBootComponentMemory
	default:
		return ImageKindOCIImage
	}
}

func validImageKind(kind string) bool {
	switch kind {
	case ImageKindOCIImage,
		ImageKindBootIndexCold,
		ImageKindBootIndexResume,
		ImageKindBootComponentRootfs,
		ImageKindBootComponentSandbox,
		ImageKindBootComponentMemory:
		return true
	default:
		return false
	}
}
