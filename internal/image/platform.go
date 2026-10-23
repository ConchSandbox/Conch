package image

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/platformutil"
)

// ComponentPlatform reads the image config, which remains authoritative for
// legacy components without descriptor platform metadata.
func ComponentPlatform(ctx context.Context, store content.Store, desc ocispec.Descriptor) (string, error) {
	config, err := images.Config(ctx, store, desc, platforms.All)
	if err != nil {
		return "", err
	}
	p, err := images.ConfigPlatform(ctx, store, config)
	if err != nil {
		return "", err
	}
	if p.OS == "" || p.Architecture == "" {
		return "", fmt.Errorf("component %s is missing OS or architecture", desc.Digest)
	}
	value, err := platformutil.Normalize(platforms.Format(p))
	if err != nil {
		return "", err
	}
	if desc.Platform != nil {
		declared, err := platformutil.Normalize(platforms.Format(*desc.Platform))
		if err != nil {
			return "", err
		}
		if declared != value {
			return "", fmt.Errorf("component %s platform %s does not match config %s", desc.Digest, declared, value)
		}
	}
	return value, nil
}

// resolvePlatformManifest selects by platform instead of index ordering and
// checks single-manifest sources too (containerd accepts those unconditionally).
func resolvePlatformManifest(ctx context.Context, store content.Store, desc ocispec.Descriptor, platform string) (ocispec.Descriptor, error) {
	p, err := platformutil.Parse(platform)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	want := platforms.Format(p)
	if images.IsManifestType(desc.MediaType) {
		actual, err := ComponentPlatform(ctx, store, desc)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		if actual != want {
			return ocispec.Descriptor{}, fmt.Errorf("image platform %s does not match requested %s", actual, want)
		}
		desc.Platform = &p
		return desc, nil
	}
	if !images.IsIndexType(desc.MediaType) {
		return ocispec.Descriptor{}, fmt.Errorf("unsupported image media type %s", desc.MediaType)
	}
	raw, err := content.ReadBlob(ctx, store, desc)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return ocispec.Descriptor{}, err
	}
	for _, child := range index.Manifests {
		if child.Platform != nil && platforms.OnlyStrict(p).Match(*child.Platform) {
			return resolvePlatformManifest(ctx, store, child, want)
		}
	}
	// Older OCI indexes may omit descriptor platforms. Resolve those from
	// their configs only after considering explicitly matching descriptors.
	for _, child := range index.Manifests {
		if child.Platform == nil {
			if resolved, err := resolvePlatformManifest(ctx, store, child, want); err == nil {
				return resolved, nil
			}
		}
	}
	return ocispec.Descriptor{}, fmt.Errorf("no manifest for platform %s", want)
}

func inspectBootIndexPlatform(ctx context.Context, store content.Store, info BootIndexInfo) (string, error) {
	value, err := ComponentPlatform(ctx, store, info.RootfsDescriptor)
	if err != nil {
		return "", err
	}
	components := []ocispec.Descriptor{info.SandboxDescriptor}
	if info.Resume {
		components = append(components, info.MemDescriptor)
	}
	for _, component := range components {
		other, err := ComponentPlatform(ctx, store, component)
		if err != nil {
			return "", err
		}
		if other != value {
			return "", fmt.Errorf("Boot Index mixes component platforms %s and %s", value, other)
		}
	}
	return value, nil
}
