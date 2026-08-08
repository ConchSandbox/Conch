package erofsconvert

import (
	"context"
	"fmt"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	imageconverter "github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/platforms"
	toolkit "github.com/erofs/erofs-container-toolkit/pkg/converter"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

func ConvertRootfs(ctx context.Context, client *containerdclient.Client, req ConvertRootfsRequest) (ConvertRootfsResult, error) {
	if client == nil || client.Client == nil {
		return ConvertRootfsResult{}, fmt.Errorf("containerd client is required")
	}
	req, err := NormalizeRequest(req)
	if err != nil {
		return ConvertRootfsResult{}, err
	}

	convertCtx := containerdclient.NewNamespaceContext(ctx)
	converted, err := imageconverter.Convert(
		convertCtx,
		client,
		req.TargetImage,
		req.SourceImage,
		imageconverter.WithDockerToOCI(true),
		imageconverter.WithPlatform(platforms.DefaultStrict()),
		imageconverter.WithLayerConvertFunc(toolkit.LayerConvertFunc(toolkitOptions(req.MkfsOptions)...)),
		imageconverter.WithUpdateManifest(normalizeToolkitErofsManifest),
	)
	if err != nil {
		return ConvertRootfsResult{}, fmt.Errorf("convert rootfs to erofs: %w", err)
	}

	manifest, err := images.Manifest(convertCtx, client.ContentStore(), converted.Target, platforms.DefaultStrict())
	if err != nil {
		return ConvertRootfsResult{}, fmt.Errorf("convert rootfs to erofs: %w", err)
	}
	layers, err := ValidateNativeLayers(manifest.Layers, req.AlignBytes)
	if err != nil {
		return ConvertRootfsResult{}, fmt.Errorf("convert rootfs to erofs: %w", err)
	}

	convertedImage := containerd.NewImage(client.Client, *converted)
	if err := convertedImage.Unpack(convertCtx, "erofs"); err != nil {
		return ConvertRootfsResult{}, fmt.Errorf("unpack converted rootfs with erofs snapshotter: %w", err)
	}
	diffIDs, err := convertedImage.RootFS(convertCtx)
	if err != nil {
		return ConvertRootfsResult{}, fmt.Errorf("resolve converted rootfs diff IDs: %w", err)
	}
	if len(diffIDs) == 0 {
		return ConvertRootfsResult{}, fmt.Errorf("converted rootfs diff IDs are empty")
	}
	snapshotKey := identity.ChainID(diffIDs).String()
	if snapshotKey == "" {
		return ConvertRootfsResult{}, fmt.Errorf("converted rootfs snapshot key is empty")
	}
	return ConvertRootfsResult{
		ImageName:      converted.Name,
		ManifestDigest: converted.Target.Digest.String(),
		SnapshotKey:    snapshotKey,
		Layers:         layers,
	}, nil
}

func normalizeToolkitErofsManifest(ctx context.Context, cs content.Store, originalDesc, convertedDesc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	var manifest ocispec.Manifest
	labels, err := imageconverter.ReadJSON(ctx, cs, &manifest, convertedDesc)
	if err != nil {
		return nil, err
	}

	changed := false
	for i := range manifest.Layers {
		if manifest.Layers[i].MediaType == ToolkitLegacyLayerMediaType {
			manifest.Layers[i].MediaType = NativeLayerMediaType
			changed = true
		}
	}
	if changed {
		updated, err := imageconverter.WriteJSON(ctx, cs, &manifest, convertedDesc, labels)
		if err != nil {
			return nil, err
		}
		convertedDesc = *updated
	}
	_ = originalDesc
	return &convertedDesc, nil
}

func toolkitOptions(mkfsOptions []string) []toolkit.Option {
	for _, opt := range mkfsOptions {
		if opt = strings.TrimSpace(opt); opt != "" {
			return []toolkit.Option{toolkit.WithExtraMkfsOption(opt)}
		}
	}
	return nil
}
