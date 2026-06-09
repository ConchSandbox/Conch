package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/util"
)

type NativeLayer struct {
	Descriptor ocispec.Descriptor
	Path       string
}

type BootIndexOptions struct {
	RootfsArchivePath string
	MemChainPaths     []string
	SandboxChainPaths []string
	Tag               string
	ArchivePath       string
}

func BuildBootIndexArchive(ctx context.Context, opts BootIndexOptions) (digest.Digest, error) {
	if opts.RootfsArchivePath == "" {
		return "", fmt.Errorf("rootfs archive path is required")
	}
	if len(opts.SandboxChainPaths) == 0 {
		return "", fmt.Errorf("sandbox snapshot chain is required")
	}
	if opts.Tag == "" {
		return "", fmt.Errorf("boot index tag is required")
	}
	if opts.ArchivePath == "" {
		return "", fmt.Errorf("boot index archive path is required")
	}

	tmpDir, err := os.MkdirTemp("", "conch-boot-index-layout-*")
	if err != nil {
		return "", fmt.Errorf("create boot index temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	layoutDir := filepath.Join(tmpDir, "layout")
	rootfsDesc, err := importFirstManifestFromArchive(opts.RootfsArchivePath, layoutDir)
	if err != nil {
		return "", fmt.Errorf("import rootfs manifest: %w", err)
	}
	rootfsDesc.Annotations = mergeAnnotations(rootfsDesc.Annotations, map[string]string{
		"io.conch.kind":                     KindRootfs,
		"org.opencontainers.image.ref.name": opts.Tag + "-rootfs",
	})

	manifests := []ocispec.Descriptor{rootfsDesc}
	if len(opts.MemChainPaths) > 0 {
		memDesc, _, err := writeNativeComponent(ctx, layoutDir, opts.MemChainPaths, KindMemSnapshot, opts.Tag+"-mem")
		if err != nil {
			return "", err
		}
		memDesc.Annotations = mergeAnnotations(memDesc.Annotations, map[string]string{
			"io.conch.kind":                     KindMemSnapshot,
			"org.opencontainers.image.ref.name": opts.Tag + "-mem",
		})
		manifests = append(manifests, memDesc)
	}

	sandboxDesc, _, err := writeNativeComponent(ctx, layoutDir, opts.SandboxChainPaths, KindSandbox, opts.Tag+"-sandbox")
	if err != nil {
		return "", err
	}
	sandboxDesc.Annotations = mergeAnnotations(sandboxDesc.Annotations, map[string]string{
		"io.conch.kind":                     KindSandbox,
		"org.opencontainers.image.ref.name": opts.Tag + "-sandbox",
	})
	manifests = append(manifests, sandboxDesc)

	indexDigest, err := writeIndex(layoutDir, manifests)
	if err != nil {
		return "", err
	}
	if err := util.TarDirectory(layoutDir, opts.ArchivePath); err != nil {
		return "", err
	}
	return indexDigest, nil
}

func writeNativeComponent(ctx context.Context, layoutDir string, paths []string, componentType, tag string) (ocispec.Descriptor, []NativeLayer, error) {
	if len(paths) == 0 {
		return ocispec.Descriptor{}, nil, fmt.Errorf("%s component has no paths", componentType)
	}
	if err := ensureLayout(layoutDir); err != nil {
		return ocispec.Descriptor{}, nil, err
	}

	workDir, err := os.MkdirTemp("", "conch-native-erofs-*")
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("create native erofs temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	layers := make([]NativeLayer, 0, len(paths))
	for i, path := range paths {
		layerPath := path
		info, err := os.Stat(path)
		if err != nil {
			return ocispec.Descriptor{}, nil, fmt.Errorf("stat %s path %s: %w", componentType, path, err)
		}
		if info.IsDir() {
			layerPath = filepath.Join(workDir, fmt.Sprintf("%s-layer-%d.erofs", componentType, i))
			if err := buildErofsLayer(ctx, path, layerPath); err != nil {
				return ocispec.Descriptor{}, nil, err
			}
		}

		desc, err := fileDescriptor(layerPath, erofsconvert.NativeLayerMediaType)
		if err != nil {
			return ocispec.Descriptor{}, nil, err
		}
		if err := installBlobFromFile(layerPath, layoutDir, desc.Digest); err != nil {
			return ocispec.Descriptor{}, nil, err
		}
		layers = append(layers, NativeLayer{Descriptor: desc, Path: layerPath})
	}

	layerDescs := make([]ocispec.Descriptor, 0, len(layers))
	diffIDs := make([]digest.Digest, 0, len(layers))
	for _, layer := range layers {
		layerDescs = append(layerDescs, layer.Descriptor)
		diffIDs = append(diffIDs, layer.Descriptor.Digest)
	}

	now := time.Now()
	history := make([]ocispec.History, 0, len(layers))
	for range layers {
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
	configDesc, err := writeBlobJSON(layoutDir, config, ocispec.MediaTypeImageConfig)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}

	manifest := ocispec.Manifest{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    layerDescs,
	}
	manifestDesc, err := writeBlobJSON(layoutDir, manifest, ocispec.MediaTypeImageManifest)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	manifestDesc.Annotations = map[string]string{
		"org.opencontainers.image.ref.name": tag,
	}
	return manifestDesc, layers, nil
}

func importFirstManifestFromArchive(archivePath, dstLayoutDir string) (ocispec.Descriptor, error) {
	tmpDir, err := os.MkdirTemp("", "conch-rootfs-layout-*")
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("create rootfs layout temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := util.Untar(archivePath, tmpDir); err != nil {
		return ocispec.Descriptor{}, err
	}

	indexBytes, err := os.ReadFile(filepath.Join(tmpDir, "index.json"))
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("read rootfs index.json: %w", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("unmarshal rootfs index.json: %w", err)
	}
	if len(index.Manifests) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("archive has no manifests")
	}
	if err := ensureLayout(dstLayoutDir); err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := copyBlobs(tmpDir, dstLayoutDir); err != nil {
		return ocispec.Descriptor{}, err
	}
	return firstManifestDescriptor(tmpDir, index.Manifests[0])
}

func firstManifestDescriptor(layoutDir string, desc ocispec.Descriptor) (ocispec.Descriptor, error) {
	switch desc.MediaType {
	case ocispec.MediaTypeImageManifest:
		return desc, nil
	case ocispec.MediaTypeImageIndex:
		raw, err := os.ReadFile(filepath.Join(layoutDir, "blobs", "sha256", desc.Digest.Encoded()))
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
		return firstManifestDescriptor(layoutDir, index.Manifests[0])
	default:
		return ocispec.Descriptor{}, fmt.Errorf("unsupported archive descriptor media type %s", desc.MediaType)
	}
}

func writeIndex(layoutDir string, manifests []ocispec.Descriptor) (digest.Digest, error) {
	if err := ensureLayout(layoutDir); err != nil {
		return "", err
	}
	index := ocispec.Index{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifests,
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal index.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), data, 0o644); err != nil {
		return "", fmt.Errorf("write index.json: %w", err)
	}
	return digest.FromBytes(data), nil
}

func ensureLayout(layoutDir string) error {
	if err := os.MkdirAll(filepath.Join(layoutDir, "blobs", "sha256"), 0o755); err != nil {
		return fmt.Errorf("create OCI blobs dir: %w", err)
	}
	layoutPath := filepath.Join(layoutDir, "oci-layout")
	if _, err := os.Stat(layoutPath); err == nil {
		return nil
	}
	if err := os.WriteFile(layoutPath, []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		return fmt.Errorf("write oci-layout: %w", err)
	}
	return nil
}

func writeBlobJSON(layoutDir string, value any, mediaType string) (ocispec.Descriptor, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	dgst := digest.FromBytes(data)
	if err := os.WriteFile(filepath.Join(layoutDir, "blobs", "sha256", dgst.Encoded()), data, 0o644); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("write blob %s: %w", dgst, err)
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: dgst, Size: int64(len(data))}, nil
}

func buildErofsLayer(ctx context.Context, srcDir, outPath string) error {
	args := []string{
		"--quiet",
		"-Enoinline_data",
		"--all-root",
		erofsconvert.DefaultMkfsOption,
		outPath,
		srcDir,
	}
	cmd := exec.CommandContext(ctx, "mkfs.erofs", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.erofs %s: %s: %w", srcDir, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func fileDescriptor(path, mediaType string) (ocispec.Descriptor, error) {
	file, err := os.Open(path)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("stat %s: %w", path, err)
	}
	digester := digest.Canonical.Digester()
	if _, err := io.Copy(digester.Hash(), file); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("hash %s: %w", path, err)
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: digester.Digest(), Size: info.Size()}, nil
}

func installBlobFromFile(srcPath, layoutDir string, dgst digest.Digest) error {
	dstPath := filepath.Join(layoutDir, "blobs", "sha256", dgst.Encoded())
	if _, err := os.Stat(dstPath); err == nil {
		return nil
	}
	return copyFile(srcPath, dstPath)
}

func copyBlobs(srcLayoutDir, dstLayoutDir string) error {
	srcBlobs := filepath.Join(srcLayoutDir, "blobs", "sha256")
	entries, err := os.ReadDir(srcBlobs)
	if err != nil {
		return fmt.Errorf("read rootfs blobs: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		srcPath := filepath.Join(srcBlobs, entry.Name())
		dstPath := filepath.Join(dstLayoutDir, "blobs", "sha256", entry.Name())
		if _, err := os.Stat(dstPath); err == nil {
			continue
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dstPath), "."+filepath.Base(dstPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", dstPath, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy %s -> %s: %w", srcPath, dstPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, dstPath, err)
	}
	cleanup = false
	return nil
}

func mergeAnnotations(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
