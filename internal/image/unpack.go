package image

import (
	"context"
	"errors"
	"fmt"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	KindRootfs      = "rootfs"
	KindSandbox     = "sandbox"
	KindMemSnapshot = "mem-snapshot"
	KindUnknown     = "unknown"
)

var ErrMissingSandbox = errors.New("missing required sandbox component")

// UnpackBootIndex validates a Boot Index by its immutable digest and unpacks
// all component manifests.
//
// The Boot Index must be fully available locally before calling: all content
// (manifests, configs, and layers) must exist in the content store.
func UnpackBootIndex(ctx context.Context, client *containerdclient.Client, bootIndexDigest string) error {
	unpackCtx, info, err := inspectBootIndex(ctx, client, bootIndexDigest)
	if err != nil {
		return err
	}
	if _, err := unpackBootIndexComponents(unpackCtx, client.Client, info); err != nil {
		return fmt.Errorf("unpack boot index %s: %w", info.BootIndexDigest, err)
	}
	return nil
}

func unpackBootIndexComponents(ctx context.Context, client *containerd.Client, info BootIndexInfo) (map[string]string, error) {
	return unpackBootIndexComponentSet(ctx, client, info, true)
}

func unpackBootIndexComponentsWithoutMemory(ctx context.Context, client *containerd.Client, info BootIndexInfo) (map[string]string, error) {
	return unpackBootIndexComponentSet(ctx, client, info, false)
}

func unpackBootIndexComponentSet(ctx context.Context, client *containerd.Client, info BootIndexInfo, includeMemory bool) (map[string]string, error) {
	if client == nil {
		return nil, fmt.Errorf("containerd client is required")
	}

	snapshotMap := make(map[string]string)

	components := []ocispec.Descriptor{info.RootfsDescriptor}
	if includeMemory && info.MemDescriptor.Digest != "" {
		components = append(components, info.MemDescriptor)
	}
	components = append(components, info.SandboxDescriptor)
	ulog.Info("Found components in Boot Index, starting unpack",
		ulog.F("count", len(components)))

	for _, manifestDesc := range components {
		kind := getKind(manifestDesc)
		subImageName := fmt.Sprintf("localhost/conch/%s-component:%s", kind, manifestDesc.Digest.Encoded())
		if err := validateNativeComponentManifest(ctx, client, kind, manifestDesc); err != nil {
			return nil, err
		}
		if err := ensureSubImage(ctx, client, subImageName, manifestDesc, kind); err != nil {
			return nil, err
		}
		snapshotID, err := unpackOneSubImage(ctx, client, "erofs", manifestDesc, kind, subImageName)
		if err != nil {
			return nil, err
		}
		snapshotMap[kind] = snapshotID
		ulog.Info("Generated SnapshotID",
			ulog.F("kind", kind),
			ulog.F("snapshot_id", snapshotID))
	}

	return snapshotMap, nil
}

func getKind(manifestDesc ocispec.Descriptor) string {
	if kind := manifestDesc.Annotations["io.conch.kind"]; kind != "" {
		return kind
	}
	return KindUnknown
}

func unpackOneSubImage(ctx context.Context, client *containerd.Client, snapshotterName string, manifestDesc ocispec.Descriptor, kind string, imageName string) (string, error) {
	subImg := containerd.NewImage(client, images.Image{
		Name:   imageName,
		Target: manifestDesc,
	})

	diffIDs, err := subImg.RootFS(ctx)
	if err != nil {
		return "", fmt.Errorf("get RootFS for %s: %w", kind, err)
	}
	if err := subImg.Unpack(ctx, snapshotterName); err != nil {
		return "", fmt.Errorf("unpack sub-image %s (kind: %s): %w", manifestDesc.Digest, kind, err)
	}
	return identity.ChainID(diffIDs).String(), nil
}

func isNativeErofsKind(kind string) bool {
	return kind == KindRootfs || kind == KindMemSnapshot || kind == KindSandbox
}

func validateNativeComponentManifest(ctx context.Context, client *containerd.Client, kind string, manifestDesc ocispec.Descriptor) error {
	manifest, err := images.Manifest(ctx, client.ContentStore(), manifestDesc, platforms.DefaultStrict())
	if err != nil {
		return fmt.Errorf("resolve native %s manifest: %w", kind, err)
	}
	if kind == KindRootfs {
		if _, err := erofsconvert.ValidateNativeLayers(manifest.Layers, erofsconvert.DefaultAlignBytes); err != nil {
			return fmt.Errorf("%s component is not native erofs: %w", kind, err)
		}
		return nil
	}
	if len(manifest.Layers) == 0 {
		return fmt.Errorf("%s component is not native erofs: manifest has no layers", kind)
	}
	for _, layer := range manifest.Layers {
		if layer.MediaType != erofsconvert.NativeLayerMediaType {
			return fmt.Errorf("%s component is not native erofs: layer %s media type %s is not %s", kind, layer.Digest, layer.MediaType, erofsconvert.NativeLayerMediaType)
		}
		if layer.Size <= 0 {
			return fmt.Errorf("%s component is not native erofs: layer %s size %d is invalid", kind, layer.Digest, layer.Size)
		}
	}
	return nil
}

func ensureSubImage(ctx context.Context, client *containerd.Client, imageName string, target ocispec.Descriptor, componentKind string) error {
	if imageName == "" {
		return fmt.Errorf("sub-image name is required")
	}
	_, err := client.ImageService().Create(ctx, images.Image{
		Name:   imageName,
		Target: target,
		Labels: map[string]string{
			ImageKindLabel: componentImageKind(componentKind),
		},
	})
	if err == nil || errdefs.IsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("create sub-image record %s: %w", imageName, err)
}
