package image

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/image/erofsconvert"
)

type BootIndexContentOptions struct {
	RootfsDescriptor ocispec.Descriptor
	KernelPath       string
	InitrdPath       string
	Tag              string
}

func BuildBootIndexInContent(ctx context.Context, store content.Store, opts BootIndexContentOptions) (ocispec.Descriptor, error) {
	if store == nil {
		return ocispec.Descriptor{}, fmt.Errorf("content store is required")
	}
	if opts.RootfsDescriptor.Digest == "" {
		return ocispec.Descriptor{}, fmt.Errorf("rootfs descriptor is required")
	}
	if opts.KernelPath == "" {
		return ocispec.Descriptor{}, fmt.Errorf("kernel path is required")
	}
	if opts.InitrdPath == "" {
		return ocispec.Descriptor{}, fmt.Errorf("initrd path is required")
	}
	if opts.Tag == "" {
		return ocispec.Descriptor{}, fmt.Errorf("boot index tag is required")
	}

	rootfsDesc, err := firstManifestDescriptorFromContent(ctx, store, opts.RootfsDescriptor)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("resolve rootfs manifest: %w", err)
	}
	rootfsDesc.Annotations = mergeAnnotations(rootfsDesc.Annotations, map[string]string{
		"io.conch.kind":                     KindRootfs,
		"org.opencontainers.image.ref.name": opts.Tag + "-rootfs",
	})

	sandboxDesc, err := buildKernelComponentInContent(ctx, store, opts.KernelPath, opts.InitrdPath, opts.Tag+"-sandbox")
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	sandboxDesc.Annotations = mergeAnnotations(sandboxDesc.Annotations, map[string]string{
		"io.conch.kind":                     KindSandbox,
		"org.opencontainers.image.ref.name": opts.Tag + "-sandbox",
	})

	return writeIndexToContent(ctx, store, []ocispec.Descriptor{rootfsDesc, sandboxDesc})
}

func buildKernelComponentInContent(ctx context.Context, store content.Store, kernelPath, initrdPath, tag string) (ocispec.Descriptor, error) {
	tmpDir, err := os.MkdirTemp("", "conch-kernel-component-*")
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("create kernel component temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	root := filepath.Join(tmpDir, "root")
	if err := os.MkdirAll(filepath.Join(root, "boot"), 0o755); err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := copyFile(kernelPath, filepath.Join(root, "boot", "vmlinuz")); err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := copyFile(initrdPath, filepath.Join(root, "data", "conch.initrd")); err != nil {
		return ocispec.Descriptor{}, err
	}
	return writeNativeComponentToContent(ctx, store, []string{root}, KindSandbox, tag)
}

func writeNativeComponentToContent(ctx context.Context, store content.Store, paths []string, componentType, tag string) (ocispec.Descriptor, error) {
	if len(paths) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("%s component has no paths", componentType)
	}

	workDir, err := os.MkdirTemp("", "conch-native-erofs-*")
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("create native erofs temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	layerDescs := make([]ocispec.Descriptor, 0, len(paths))
	diffIDs := make([]digest.Digest, 0, len(paths))
	for i, path := range paths {
		layerPath := path
		info, err := os.Stat(path)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("stat %s path %s: %w", componentType, path, err)
		}
		if info.IsDir() {
			layerPath = filepath.Join(workDir, fmt.Sprintf("%s-layer-%d.erofs", componentType, i))
			if err := buildErofsLayer(ctx, path, layerPath); err != nil {
				return ocispec.Descriptor{}, err
			}
		}

		desc, err := fileDescriptor(layerPath, erofsconvert.NativeLayerMediaType)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		if err := writeFileBlobToContent(ctx, store, layerPath, desc); err != nil {
			return ocispec.Descriptor{}, err
		}
		layerDescs = append(layerDescs, desc)
		diffIDs = append(diffIDs, desc.Digest)
	}

	now := time.Now()
	history := make([]ocispec.History, 0, len(layerDescs))
	for range layerDescs {
		history = append(history, ocispec.History{
			Created:    &now,
			CreatedBy:  "conch native erofs " + componentType,
			EmptyLayer: false,
		})
	}
	config := ocispec.Image{
		Created: &now,
		Platform: ocispec.Platform{
			Architecture: runtime.GOARCH,
			OS:           runtime.GOOS,
		},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: diffIDs,
		},
		Config: ocispec.ImageConfig{
			Labels: map[string]string{
				"io.conch.component.type": componentType,
			},
		},
		History: history,
	}
	configDesc, err := writeBlobJSONToContent(ctx, store, config, ocispec.MediaTypeImageConfig)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	manifest := ocispec.Manifest{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    layerDescs,
	}
	manifestDesc, err := writeBlobJSONToContent(ctx, store, manifest, ocispec.MediaTypeImageManifest)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	manifestDesc.Annotations = map[string]string{
		"org.opencontainers.image.ref.name": tag,
	}
	return manifestDesc, nil
}

func firstManifestDescriptorFromContent(ctx context.Context, store content.Store, desc ocispec.Descriptor) (ocispec.Descriptor, error) {
	switch desc.MediaType {
	case ocispec.MediaTypeImageManifest:
		return desc, nil
	case ocispec.MediaTypeImageIndex:
		raw, err := content.ReadBlob(ctx, store, desc)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("read nested index %s: %w", desc.Digest, err)
		}
		var index ocispec.Index
		if err := json.Unmarshal(raw, &index); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("unmarshal nested index %s: %w", desc.Digest, err)
		}
		if len(index.Manifests) == 0 {
			return ocispec.Descriptor{}, fmt.Errorf("nested index %s has no manifests", desc.Digest)
		}
		return firstManifestDescriptorFromContent(ctx, store, index.Manifests[0])
	default:
		return ocispec.Descriptor{}, fmt.Errorf("unsupported rootfs descriptor media type %s", desc.MediaType)
	}
}

func writeIndexToContent(ctx context.Context, store content.Store, manifests []ocispec.Descriptor) (ocispec.Descriptor, error) {
	index := ocispec.Index{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifests,
	}
	return writeBlobJSONToContent(ctx, store, index, ocispec.MediaTypeImageIndex)
}

func writeBlobJSONToContent(ctx context.Context, store content.Store, value any, mediaType string) (ocispec.Descriptor, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := content.WriteBlob(ctx, store, contentRef("json", desc.Digest), bytes.NewReader(data), desc); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("write blob %s: %w", desc.Digest, err)
	}
	return desc, nil
}

func writeFileBlobToContent(ctx context.Context, store content.Store, path string, desc ocispec.Descriptor) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	if err := content.WriteBlob(ctx, store, contentRef("file", desc.Digest), file, desc); err != nil {
		return fmt.Errorf("write blob %s: %w", desc.Digest, err)
	}
	return nil
}

func contentRef(kind string, dgst digest.Digest) string {
	return "conch-" + kind + "-" + dgst.String()
}
