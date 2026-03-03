package image

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/daemon"
)

// GetSnapshotID resolves image name and returns rootfs snapshot ID (chainID)
// Supports both regular images and OCI Image Index (multi-architecture images)
func GetSnapshotID(ctx context.Context, client *daemon.Client, namespace, imageName string) (string, error) {
	nsCtx, err := client.WithNamespace(ctx, namespace)
	if err != nil {
		return "", fmt.Errorf("create namespace context: %w", err)
	}

	imageMeta, err := client.ImageService().Get(nsCtx, imageName)
	if err != nil {
		return "", fmt.Errorf("get image by name %s: %w", imageName, err)
	}

	imageID := imageMeta.Target.Digest.String()
	if imageID == "" {
		return "", fmt.Errorf("image digest empty for image name %s", imageName)
	}

	imageDigest, err := digest.Parse(imageID)
	if err != nil {
		return "", fmt.Errorf("parse image digest: %w", err)
	}

	// Try to get layer chain IDs from RootFS
	desc := ocispec.Descriptor{Digest: imageDigest}
	rootfs, rootfsErr := images.RootFS(nsCtx, client.ContentStore(), desc)

	// If RootFS is empty, try parsing from config (handles Image Index scenario)
	if len(rootfs) == 0 {
		configDesc, err := resolveConfigDesc(nsCtx, client.ContentStore(), desc)
		if err != nil {
			if rootfsErr != nil {
				return "", fmt.Errorf("get rootfs from image %s: %w", imageID, rootfsErr)
			}
			return "", err
		}

		configBlob, err := content.ReadBlob(nsCtx, client.ContentStore(), configDesc)
		if err != nil {
			return "", fmt.Errorf("read image config for %s: %w", imageID, err)
		}

		var img ocispec.Image
		if err := json.Unmarshal(configBlob, &img); err != nil {
			return "", fmt.Errorf("unmarshal image config for %s: %w", imageID, err)
		}
		rootfs = img.RootFS.DiffIDs
	}

	if len(rootfs) == 0 {
		return "", fmt.Errorf("image %s rootfs diffIDs empty", imageID)
	}

	chainID := identity.ChainID(rootfs)
	snapshotID := chainID.String()
	if snapshotID == "" {
		return "", fmt.Errorf("snapshot id empty for image %s", imageID)
	}
	return snapshotID, nil
}

// manifestProbe parses manifest blob for platform and config info
type manifestProbe struct {
	MediaType string               `json:"mediaType"`
	Config    ocispec.Descriptor   `json:"config"`
	Manifests []ocispec.Descriptor `json:"manifests"`
}

// resolveConfigDesc parses image manifest and returns config descriptor
// Handles OCI Image Index (multi-architecture manifest lists)
func resolveConfigDesc(ctx context.Context, store content.Store, desc ocispec.Descriptor) (ocispec.Descriptor, error) {
	blob, err := content.ReadBlob(ctx, store, desc)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("read manifest blob: %w", err)
	}

	var probe manifestProbe
	if err := json.Unmarshal(blob, &probe); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("unmarshal manifest blob: %w", err)
	}

	// Handle image index (multi-architecture manifests)
	if len(probe.Manifests) > 0 {
		matcher := platforms.Default()
		var chosen *ocispec.Descriptor
		for i := range probe.Manifests {
			m := probe.Manifests[i]
			if m.Platform != nil && matcher.Match(*m.Platform) {
				chosen = &m
				break
			}
		}
		// If no matching platform and only one manifest, use it
		if chosen == nil && len(probe.Manifests) == 1 {
			chosen = &probe.Manifests[0]
		}
		if chosen == nil {
			return ocispec.Descriptor{}, fmt.Errorf("no matching manifest for platform %s", platforms.Format(platforms.DefaultSpec()))
		}
		return resolveConfigDesc(ctx, store, ocispec.Descriptor{Digest: chosen.Digest, MediaType: chosen.MediaType})
	}

	if probe.Config.Digest == "" {
		return ocispec.Descriptor{}, fmt.Errorf("config digest empty in manifest %s", desc.Digest.String())
	}
	return probe.Config, nil
}
