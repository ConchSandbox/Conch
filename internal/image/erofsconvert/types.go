package erofsconvert

import (
	"context"
	"fmt"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	NativeLayerMediaType        = "application/vnd.erofs.layer.v1"
	ToolkitLegacyLayerMediaType = "application/vnd.erofs"
	DefaultAlignBytes           = int64(2 * 1024 * 1024)
	DefaultMkfsOption           = "--fsalignblks=512"
)

type RootfsErofsConverter interface {
	Convert(ctx context.Context, req ConvertRootfsRequest) (ConvertRootfsResult, error)
}

type ConvertRootfsRequest struct {
	Namespace   string   `json:"namespace,omitempty"`
	SourceImage string   `json:"source_image"`
	TargetImage string   `json:"target_image"`
	MkfsOptions []string `json:"mkfs_options,omitempty"`
	AlignBytes  int64    `json:"align_bytes,omitempty"`
}

type ConvertRootfsResult struct {
	ImageName      string       `json:"image_name"`
	ManifestDigest string       `json:"manifest_digest"`
	SnapshotKey    string       `json:"snapshot_key,omitempty"`
	Layers         []ErofsLayer `json:"layers"`
}

type ErofsLayer struct {
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

func NormalizeRequest(req ConvertRootfsRequest) (ConvertRootfsRequest, error) {
	req.Namespace = strings.TrimSpace(req.Namespace)
	req.SourceImage = strings.TrimSpace(req.SourceImage)
	req.TargetImage = strings.TrimSpace(req.TargetImage)
	if req.Namespace == "" {
		return ConvertRootfsRequest{}, fmt.Errorf("namespace is required")
	}
	if req.SourceImage == "" {
		return ConvertRootfsRequest{}, fmt.Errorf("source image is required")
	}
	if req.TargetImage == "" {
		return ConvertRootfsRequest{}, fmt.Errorf("target image is required")
	}
	if req.AlignBytes == 0 {
		req.AlignBytes = DefaultAlignBytes
	}
	if req.AlignBytes < 0 {
		return ConvertRootfsRequest{}, fmt.Errorf("align bytes must be positive")
	}
	req.MkfsOptions = NormalizeMkfsOptions(req.MkfsOptions)
	return req, nil
}

func NormalizeMkfsOptions(opts []string) []string {
	out := make([]string, 0, len(opts)+1)
	seenDefault := false
	for _, opt := range opts {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		if opt == DefaultMkfsOption {
			seenDefault = true
		}
		out = append(out, opt)
	}
	if !seenDefault {
		out = append([]string{DefaultMkfsOption}, out...)
	}
	return out
}

func ValidateNativeLayers(layers []ocispec.Descriptor, alignBytes int64) ([]ErofsLayer, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("converted manifest has no layers")
	}
	if alignBytes == 0 {
		alignBytes = DefaultAlignBytes
	}
	if alignBytes < 0 {
		return nil, fmt.Errorf("align bytes must be positive")
	}

	out := make([]ErofsLayer, 0, len(layers))
	for _, layer := range layers {
		if layer.MediaType != NativeLayerMediaType {
			return nil, fmt.Errorf("layer %s media type %s is not %s", layer.Digest, layer.MediaType, NativeLayerMediaType)
		}
		if layer.Size <= 0 {
			return nil, fmt.Errorf("layer %s size %d is invalid", layer.Digest, layer.Size)
		}
		if layer.Size%alignBytes != 0 {
			return nil, fmt.Errorf("layer %s size %d is not aligned to %d", layer.Digest, layer.Size, alignBytes)
		}
		out = append(out, ErofsLayer{
			Digest:    layer.Digest.String(),
			MediaType: layer.MediaType,
			Size:      layer.Size,
		})
	}
	return out, nil
}
