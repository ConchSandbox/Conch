package erofsconvert

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	imageconverter "github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	toolkit "github.com/erofs/erofs-container-toolkit/pkg/converter"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ToolkitConverter struct {
	client imageconverter.Client
}

func NewToolkitConverter(client imageconverter.Client) *ToolkitConverter {
	return &ToolkitConverter{
		client: client,
	}
}

func (c *ToolkitConverter) Convert(ctx context.Context, req ConvertRootfsRequest) (ConvertRootfsResult, error) {
	if c == nil || c.client == nil {
		return ConvertRootfsResult{}, fmt.Errorf("containerd client is required")
	}
	req, err := NormalizeRequest(req)
	if err != nil {
		return ConvertRootfsResult{}, err
	}

	convertCtx := namespaces.WithNamespace(ctx, req.Namespace)
	converted, err := imageconverter.Convert(
		convertCtx,
		c.client,
		req.TargetImage,
		req.SourceImage,
		imageconverter.WithDockerToOCI(true),
		imageconverter.WithPlatform(platforms.DefaultStrict()),
		imageconverter.WithLayerConvertFunc(toolkit.LayerConvertFunc(toolkitOptions(req.MkfsOptions)...)),
		imageconverter.WithUpdateManifest(normalizeToolkitErofsManifest),
	)
	if err != nil {
		return ConvertRootfsResult{}, err
	}

	manifest, err := images.Manifest(convertCtx, c.client.ContentStore(), converted.Target, platforms.DefaultStrict())
	if err != nil {
		return ConvertRootfsResult{}, err
	}
	layers, err := ValidateNativeLayers(manifest.Layers, req.AlignBytes)
	if err != nil {
		return ConvertRootfsResult{}, err
	}

	return ConvertRootfsResult{
		ImageName:      converted.Name,
		ManifestDigest: converted.Target.Digest.String(),
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
