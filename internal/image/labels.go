package image

import (
	"context"
	"encoding/json"
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

// DetectImageKind classifies an image using only its top-level
// manifest or index. A Conch Boot Index is identified by its io.conch.* index
// or component-descriptor annotations; referenced manifests, configs, and
// layers do not need to be present in the content store.
//
// Once an index carries Conch metadata, malformed component topology is
// reported as an error instead of being treated as an ordinary OCI index.
func DetectImageKind(ctx context.Context, store content.Store, target ocispec.Descriptor) (string, error) {
	if target.MediaType != ocispec.MediaTypeImageIndex {
		return ImageKindOCIImage, nil
	}
	if store == nil {
		return "", fmt.Errorf("content store is required")
	}
	raw, err := content.ReadBlob(ctx, store, target)
	if err != nil {
		return "", fmt.Errorf("read image index %s: %w", target.Digest, err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return "", ErrInvalidContent.Wrap(fmt.Errorf("unmarshal image index %s: %w", target.Digest, err))
	}
	if index.MediaType != "" && index.MediaType != ocispec.MediaTypeImageIndex {
		return "", ErrInvalidContent.Wrap(fmt.Errorf("image index %s declares media type %q", target.Digest, index.MediaType))
	}
	if !hasConchIndexMetadata(index) {
		return ImageKindOCIImage, nil
	}
	info, err := inspectBootIndexMetadata(target, index)
	if err != nil {
		return "", ErrInvalidContent.Wrap(fmt.Errorf("invalid Conch Boot Index %s: %w", target.Digest, err))
	}
	if info.Resume {
		return ImageKindBootIndexResume, nil
	}
	return ImageKindBootIndexCold, nil
}

func hasConchIndexMetadata(index ocispec.Index) bool {
	for key := range index.Annotations {
		if strings.HasPrefix(key, "io.conch.") {
			return true
		}
	}
	for _, manifest := range index.Manifests {
		for key := range manifest.Annotations {
			if strings.HasPrefix(key, "io.conch.") {
				return true
			}
		}
	}
	return false
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
