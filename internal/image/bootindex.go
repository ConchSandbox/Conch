package image

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	continuityfs "github.com/containerd/continuity/fs"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
)

const (
	AnnotationVMM          = "io.conch.vmm"
	AnnotationMemorySizeMB = "io.conch.memory-size-mb"
)

func withBootIndexLease(ctx context.Context, client *containerdclient.Client, bootIndexDigest string, operation func(context.Context) error) (retErr error) {
	leaseCtx, done, err := client.WithLease(containerdclient.NewNamespaceContext(ctx))
	if err != nil {
		return fmt.Errorf("create Boot Index operation lease: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(
			containerdclient.NewNamespaceContext(context.WithoutCancel(ctx)),
			10*time.Second,
		)
		defer cancel()
		if err := done(cleanupCtx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("delete Boot Index operation lease: %w", err))
		}
	}()
	leaseID, ok := leases.FromContext(leaseCtx)
	if !ok {
		return fmt.Errorf("Boot Index operation lease is missing from context")
	}
	lease := leases.Lease{ID: leaseID}
	if err := client.LeasesService().AddResource(leaseCtx, lease, leases.Resource{Type: "content", ID: bootIndexDigest}); err != nil {
		return fmt.Errorf("retain Boot Index %s: %w", bootIndexDigest, err)
	}
	return operation(leaseCtx)
}

type BootIndexContentOptions struct {
	RootfsDescriptor  ocispec.Descriptor
	MemDescriptor     ocispec.Descriptor
	SandboxDescriptor ocispec.Descriptor
	KernelPath        string
	InitrdPath        string
	VMMName           string
	MemorySizeMB      int64
}

func BuildBootIndexInContent(ctx context.Context, store content.Store, opts BootIndexContentOptions) (ocispec.Descriptor, error) {
	if store == nil {
		return ocispec.Descriptor{}, fmt.Errorf("content store is required")
	}
	if !descriptorProvided(opts.RootfsDescriptor) {
		return ocispec.Descriptor{}, fmt.Errorf("rootfs descriptor is required")
	}
	preparedSandbox := descriptorProvided(opts.SandboxDescriptor)
	kernelPath := strings.TrimSpace(opts.KernelPath)
	initrdPath := strings.TrimSpace(opts.InitrdPath)
	hasKernelAssets := kernelPath != "" || initrdPath != ""
	if preparedSandbox == hasKernelAssets {
		return ocispec.Descriptor{}, fmt.Errorf("exactly one sandbox source is required: prepared descriptor or kernel/initrd paths")
	}
	if hasKernelAssets && (kernelPath == "" || initrdPath == "") {
		return ocispec.Descriptor{}, fmt.Errorf("kernel and initrd paths must be provided together")
	}
	hasMem := descriptorProvided(opts.MemDescriptor)
	vmmName := strings.TrimSpace(opts.VMMName)
	if hasMem && vmmName == "" {
		return ocispec.Descriptor{}, fmt.Errorf("VMM name is required for a mem-snapshot component")
	}
	if !hasMem && vmmName != "" {
		return ocispec.Descriptor{}, fmt.Errorf("VMM name requires a mem-snapshot component")
	}
	if hasMem && opts.MemorySizeMB <= 0 {
		return ocispec.Descriptor{}, fmt.Errorf("positive memory size is required for a mem-snapshot component")
	}
	if !hasMem && opts.MemorySizeMB != 0 {
		return ocispec.Descriptor{}, fmt.Errorf("memory size requires a mem-snapshot component")
	}

	rootfsDesc, err := normalizeComponentDescriptor(ctx, store, opts.RootfsDescriptor, KindRootfs, "")
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("resolve rootfs manifest: %w", err)
	}

	manifests := []ocispec.Descriptor{rootfsDesc}
	if hasMem {
		memDesc, err := normalizeComponentDescriptor(ctx, store, opts.MemDescriptor, KindMemSnapshot, vmmName)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("resolve mem-snapshot manifest: %w", err)
		}
		memDesc.Annotations = mergeAnnotations(memDesc.Annotations, map[string]string{
			AnnotationMemorySizeMB: strconv.FormatInt(opts.MemorySizeMB, 10),
		})
		manifests = append(manifests, memDesc)
	}

	var sandboxDesc ocispec.Descriptor
	if preparedSandbox {
		sandboxDesc, err = normalizeComponentDescriptor(ctx, store, opts.SandboxDescriptor, KindSandbox, "")
	} else {
		sandboxDesc, err = buildKernelComponentInContent(ctx, store, kernelPath, initrdPath)
	}
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("resolve sandbox manifest: %w", err)
	}
	manifests = append(manifests, sandboxDesc)

	indexAnnotations := map[string]string(nil)
	if vmmName != "" {
		indexAnnotations = map[string]string{
			AnnotationVMM:          vmmName,
			AnnotationMemorySizeMB: strconv.FormatInt(opts.MemorySizeMB, 10),
		}
	}
	return writeIndexToContent(ctx, store, manifests, indexAnnotations)
}

func buildKernelComponentInContent(ctx context.Context, store content.Store, kernelPath, initrdPath string) (ocispec.Descriptor, error) {
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
	if err := continuityfs.CopyFile(filepath.Join(root, "boot", "vmlinuz"), kernelPath); err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := continuityfs.CopyFile(filepath.Join(root, "data", "conch.initrd"), initrdPath); err != nil {
		return ocispec.Descriptor{}, err
	}
	return BuildNativeComponentInContent(ctx, store, []string{root}, KindSandbox)
}

// BuildNativeComponentInContent writes regular native EROFS files and/or
// directories converted to native EROFS layers into store, then publishes the
// component manifest and config. It accepts only the three supported Conch
// component kinds and rejects symlink/special-file inputs.
func BuildNativeComponentInContent(ctx context.Context, store content.Store, paths []string, kind string) (ocispec.Descriptor, error) {
	if store == nil {
		return ocispec.Descriptor{}, fmt.Errorf("content store is required")
	}
	if !isNativeErofsKind(kind) {
		return ocispec.Descriptor{}, fmt.Errorf("unsupported native component kind %q", kind)
	}
	if len(paths) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("%s component has no paths", kind)
	}

	workDir, err := os.MkdirTemp("", "conch-native-erofs-*")
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("create native erofs temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	layerDescs := make([]ocispec.Descriptor, 0, len(paths))
	diffIDs := make([]digest.Digest, 0, len(paths))
	for i, rawPath := range paths {
		path := strings.TrimSpace(rawPath)
		if path == "" {
			return ocispec.Descriptor{}, fmt.Errorf("%s component path %d is empty", kind, i)
		}
		linkInfo, err := os.Lstat(path)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("stat %s path %s: %w", kind, path, err)
		}
		if linkInfo.Mode()&os.ModeSymlink != 0 {
			return ocispec.Descriptor{}, fmt.Errorf("%s component path %s is a symlink", kind, path)
		}
		layerPath := path
		info, err := os.Stat(path)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("stat %s path %s: %w", kind, path, err)
		}
		if info.IsDir() {
			layerPath = filepath.Join(workDir, fmt.Sprintf("%s-layer-%d.erofs", kind, i))
			if err := buildErofsLayer(ctx, path, layerPath); err != nil {
				return ocispec.Descriptor{}, err
			}
		} else if !info.Mode().IsRegular() {
			return ocispec.Descriptor{}, fmt.Errorf("%s component path %s is not a regular file or directory", kind, path)
		}

		desc, err := writeFileBlobToContent(ctx, store, layerPath, erofsconvert.NativeLayerMediaType)
		if err != nil {
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
			CreatedBy:  "conch native erofs " + kind,
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
				"io.conch.component.type": kind,
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
		"io.conch.kind":                     kind,
		"org.opencontainers.image.ref.name": canonicalComponentRef(kind, manifestDesc.Digest),
	}
	return manifestDesc, nil
}

func descriptorProvided(desc ocispec.Descriptor) bool {
	return desc.Digest != "" || desc.MediaType != "" || desc.Size != 0 || len(desc.Annotations) != 0
}

func normalizeComponentDescriptor(
	ctx context.Context,
	store content.Store,
	desc ocispec.Descriptor,
	expectedKind, vmmName string,
) (ocispec.Descriptor, error) {
	if err := validateDescriptor(desc, expectedKind+" input"); err != nil {
		return ocispec.Descriptor{}, err
	}
	if kind := getKind(desc); kind != KindUnknown && kind != expectedKind {
		return ocispec.Descriptor{}, fmt.Errorf("%s descriptor has component kind %q", expectedKind, kind)
	}
	manifest, err := firstManifestDescriptorFromContent(ctx, store, desc)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if kind := getKind(manifest); kind != KindUnknown && kind != expectedKind {
		return ocispec.Descriptor{}, fmt.Errorf("%s manifest has component kind %q", expectedKind, kind)
	}
	if manifest.MediaType != ocispec.MediaTypeImageManifest {
		return ocispec.Descriptor{}, fmt.Errorf("%s component has media type %q, want %q", expectedKind, manifest.MediaType, ocispec.MediaTypeImageManifest)
	}
	if err := validateContentClosure(ctx, store, manifest); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("validate %s component closure: %w", expectedKind, err)
	}
	annotations := map[string]string{
		"io.conch.kind":                     expectedKind,
		"org.opencontainers.image.ref.name": canonicalComponentRef(expectedKind, manifest.Digest),
	}
	if vmmName != "" {
		annotations[AnnotationVMM] = vmmName
	}
	manifest.Annotations = mergeAnnotations(manifest.Annotations, annotations)
	return manifest, nil
}

func canonicalComponentRef(kind string, componentDigest digest.Digest) string {
	return fmt.Sprintf(
		"localhost/conch/%s-component:%s-%s",
		kind,
		componentDigest.Algorithm(),
		componentDigest.Encoded(),
	)
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

func writeIndexToContent(ctx context.Context, store content.Store, manifests []ocispec.Descriptor, annotations map[string]string) (ocispec.Descriptor, error) {
	index := ocispec.Index{
		Versioned:   ispec.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageIndex,
		Manifests:   manifests,
		Annotations: annotations,
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

func writeFileBlobToContent(ctx context.Context, store content.Store, path, mediaType string) (ocispec.Descriptor, error) {
	file, err := os.Open(path)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("stat %s: %w", path, err)
	}

	desc := ocispec.Descriptor{MediaType: mediaType, Size: info.Size()}
	writer, err := content.OpenWriter(
		ctx,
		store,
		content.WithRef(contentRef("file-upload", digest.FromString(path))),
		content.WithDescriptor(desc),
	)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("open content writer for %s: %w", path, err)
	}
	defer writer.Close()
	if err := writer.Truncate(0); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("truncate content writer for %s: %w", path, err)
	}
	if _, err := io.Copy(writer, file); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("write %s to content store: %w", path, err)
	}
	desc.Digest = writer.Digest()
	if err := writer.Commit(ctx, desc.Size, desc.Digest); err != nil && !errdefs.IsAlreadyExists(err) {
		return ocispec.Descriptor{}, fmt.Errorf("commit blob %s: %w", desc.Digest, err)
	}
	return desc, nil
}

func contentRef(kind string, dgst digest.Digest) string {
	return "conch-" + kind + "-" + dgst.String()
}

// InspectBootIndex resolves and validates a Boot Index directly by digest. It
// does not create image records or snapshots.
func InspectBootIndex(ctx context.Context, client *containerdclient.Client, bootIndexDigest string) (BootIndexInfo, error) {
	_, info, err := inspectBootIndex(ctx, client, bootIndexDigest)
	return info, err
}

// InspectBootIndexReference validates the complete Boot Index closure named
// by a local image record without unpacking any component snapshots.
func InspectBootIndexReference(ctx context.Context, client *containerdclient.Client, reference string) (BootIndexInfo, error) {
	if client == nil || client.Client == nil {
		return BootIndexInfo{}, fmt.Errorf("containerd client is required")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return BootIndexInfo{}, fmt.Errorf("%w: reference is required", ErrInvalidArgument)
	}
	inspectCtx := containerdclient.NewNamespaceContext(ctx)
	img, err := client.GetImage(inspectCtx, reference)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return BootIndexInfo{}, ErrNotFound.Wrap(err)
		}
		return BootIndexInfo{}, fmt.Errorf("lookup boot index reference %s: %w", reference, err)
	}
	info, err := InspectBootIndexContent(inspectCtx, client.ContentStore(), img.Target())
	if err != nil {
		return BootIndexInfo{}, fmt.Errorf("inspect boot index reference %s: %w", reference, err)
	}
	return info, nil
}

func inspectBootIndex(
	ctx context.Context,
	client *containerdclient.Client,
	bootIndexDigest string,
) (context.Context, BootIndexInfo, error) {
	if client == nil || client.Client == nil {
		return nil, BootIndexInfo{}, fmt.Errorf("containerd client is required")
	}
	if strings.TrimSpace(bootIndexDigest) == "" {
		return nil, BootIndexInfo{}, fmt.Errorf("%w: boot_index_digest is required", ErrInvalidArgument)
	}
	resolveCtx := containerdclient.NewNamespaceContext(ctx)
	_, info, err := inspectBootIndexByDigest(resolveCtx, client.ContentStore(), bootIndexDigest)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, BootIndexInfo{}, ErrNotFound.Wrap(err)
		}
		return nil, BootIndexInfo{}, err
	}
	return resolveCtx, info, nil
}

// inspectBootIndexByDigest resolves and validates a Boot Index directly from
// the content store, without relying on a mutable containerd image name.
func inspectBootIndexByDigest(ctx context.Context, store content.Store, rawDigest string) (ocispec.Descriptor, BootIndexInfo, error) {
	if store == nil {
		return ocispec.Descriptor{}, BootIndexInfo{}, fmt.Errorf("content store is required")
	}
	dgst, err := digest.Parse(strings.TrimSpace(rawDigest))
	if err != nil {
		return ocispec.Descriptor{}, BootIndexInfo{}, ErrInvalidContent.Wrap(fmt.Errorf("invalid boot index digest %q: %w", rawDigest, err))
	}
	contentInfo, err := store.Info(ctx, dgst)
	if err != nil {
		return ocispec.Descriptor{}, BootIndexInfo{}, fmt.Errorf("resolve boot index content %s: %w", dgst, err)
	}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    dgst,
		Size:      contentInfo.Size,
	}
	info, err := InspectBootIndexContent(ctx, store, desc)
	if err != nil {
		return ocispec.Descriptor{}, BootIndexInfo{}, err
	}
	return desc, info, nil
}

// InspectBootIndexContent validates the complete descriptor closure rooted at
// desc and returns the typed Conch components. It rejects unknown and duplicate
// component kinds before the index can be used for startup.
func InspectBootIndexContent(ctx context.Context, store content.Store, desc ocispec.Descriptor) (BootIndexInfo, error) {
	if store == nil {
		return BootIndexInfo{}, fmt.Errorf("content store is required")
	}
	if desc.MediaType != ocispec.MediaTypeImageIndex {
		return BootIndexInfo{}, ErrInvalidContent.Wrap(fmt.Errorf("boot index %s has media type %q, want %q", desc.Digest, desc.MediaType, ocispec.MediaTypeImageIndex))
	}
	if err := validateDescriptor(desc, "boot index"); err != nil {
		return BootIndexInfo{}, ErrInvalidContent.Wrap(err)
	}

	raw, err := content.ReadBlob(ctx, store, desc)
	if err != nil {
		return BootIndexInfo{}, fmt.Errorf("read boot index %s: %w", desc.Digest, err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return BootIndexInfo{}, ErrInvalidContent.Wrap(fmt.Errorf("unmarshal boot index %s: %w", desc.Digest, err))
	}
	if index.MediaType != "" && index.MediaType != ocispec.MediaTypeImageIndex {
		return BootIndexInfo{}, ErrInvalidContent.Wrap(fmt.Errorf("boot index %s declares media type %q", desc.Digest, index.MediaType))
	}

	info, err := inspectBootIndexMetadata(desc, index)
	if err != nil {
		return BootIndexInfo{}, ErrInvalidContent.Wrap(err)
	}
	if err := validateContentClosure(ctx, store, desc); err != nil {
		return BootIndexInfo{}, fmt.Errorf("validate boot index %s closure: %w", desc.Digest, err)
	}
	return info, nil
}

// inspectBootIndexMetadata validates the metadata carried by the top-level
// index and its component descriptors without reading referenced content.
func inspectBootIndexMetadata(desc ocispec.Descriptor, index ocispec.Index) (BootIndexInfo, error) {
	components, err := validateBootIndexManifestKinds(index.Manifests)
	if err != nil {
		return BootIndexInfo{}, err
	}
	info := BootIndexInfo{
		BootIndexDigest:   desc.Digest.String(),
		RootfsDescriptor:  components[KindRootfs],
		MemDescriptor:     components[KindMemSnapshot],
		SandboxDescriptor: components[KindSandbox],
	}
	info.Resume = info.MemDescriptor.Digest != ""
	indexVMM := strings.TrimSpace(index.Annotations[AnnotationVMM])
	memVMM := strings.TrimSpace(info.MemDescriptor.Annotations[AnnotationVMM])
	indexMemorySize := strings.TrimSpace(index.Annotations[AnnotationMemorySizeMB])
	memMemorySize := strings.TrimSpace(info.MemDescriptor.Annotations[AnnotationMemorySizeMB])
	if info.Resume {
		if indexVMM == "" || memVMM == "" {
			return BootIndexInfo{}, fmt.Errorf("resume boot index %s is missing %s capability", desc.Digest, AnnotationVMM)
		}
		if indexVMM != memVMM {
			return BootIndexInfo{}, fmt.Errorf("boot index VMM %q does not match mem component VMM %q", indexVMM, memVMM)
		}
		info.VMMName = indexVMM
		switch {
		case indexMemorySize == "" && memMemorySize == "":
			// Legacy Cloud Hypervisor indexes can still derive the size from
			// mem.img. StratoVirt's memory artifact is not a raw RAM file.
			if indexVMM == "stratovirt" {
				return BootIndexInfo{}, fmt.Errorf("resume boot index %s is missing %s", desc.Digest, AnnotationMemorySizeMB)
			}
		case indexMemorySize == "" || memMemorySize == "":
			return BootIndexInfo{}, fmt.Errorf("resume boot index %s has incomplete %s metadata", desc.Digest, AnnotationMemorySizeMB)
		default:
			if indexMemorySize != memMemorySize {
				return BootIndexInfo{}, fmt.Errorf("boot index memory size %q does not match mem component memory size %q", indexMemorySize, memMemorySize)
			}
			memorySizeMB, parseErr := strconv.ParseInt(indexMemorySize, 10, 64)
			if parseErr != nil || memorySizeMB <= 0 {
				return BootIndexInfo{}, fmt.Errorf("boot index has invalid %s value %q", AnnotationMemorySizeMB, indexMemorySize)
			}
			info.MemorySizeMB = memorySizeMB
		}
	} else if indexVMM != "" {
		return BootIndexInfo{}, fmt.Errorf("cold boot index %s has unexpected %s capability", desc.Digest, AnnotationVMM)
	} else if indexMemorySize != "" || memMemorySize != "" {
		return BootIndexInfo{}, fmt.Errorf("cold boot index %s has unexpected %s capability", desc.Digest, AnnotationMemorySizeMB)
	}
	return info, nil
}

func validateDescriptor(desc ocispec.Descriptor, name string) error {
	if desc.Digest == "" {
		return fmt.Errorf("%s descriptor digest is required", name)
	}
	if err := desc.Digest.Validate(); err != nil {
		return fmt.Errorf("invalid %s descriptor digest %q: %w", name, desc.Digest, err)
	}
	if desc.Size < 0 {
		return fmt.Errorf("%s descriptor size %d is invalid", name, desc.Size)
	}
	return nil
}

func validateContentClosure(ctx context.Context, store content.Store, root ocispec.Descriptor) error {
	children := images.ChildrenHandler(store)
	handler := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if err := validateDescriptor(desc, "content"); err != nil {
			return nil, ErrInvalidContent.Wrap(err)
		}
		info, err := store.Info(ctx, desc.Digest)
		if err != nil {
			return nil, fmt.Errorf("content %s is unavailable: %w", desc.Digest, err)
		}
		if desc.Size > 0 && info.Size != desc.Size {
			return nil, ErrInvalidContent.Wrap(fmt.Errorf("content %s size %d does not match descriptor size %d", desc.Digest, info.Size, desc.Size))
		}
		return children.Handle(ctx, desc)
	})
	return images.WalkNotEmpty(ctx, handler, root)
}

func validateBootIndexManifestKinds(manifests []ocispec.Descriptor) (map[string]ocispec.Descriptor, error) {
	components := make(map[string]ocispec.Descriptor, len(manifests))
	for _, manifest := range manifests {
		kind := getKind(manifest)
		if !isNativeErofsKind(kind) {
			return nil, fmt.Errorf("unsupported boot index component kind %q", kind)
		}
		if _, exists := components[kind]; exists {
			return nil, fmt.Errorf("boot index has duplicate component kind %q", kind)
		}
		if manifest.MediaType != ocispec.MediaTypeImageManifest {
			return nil, fmt.Errorf("boot index component %q has media type %q, want %q", kind, manifest.MediaType, ocispec.MediaTypeImageManifest)
		}
		if err := validateDescriptor(manifest, kind+" component"); err != nil {
			return nil, err
		}
		components[kind] = manifest
	}
	if components[KindRootfs].Digest == "" {
		return nil, fmt.Errorf("boot index missing required kind %q", KindRootfs)
	}
	if components[KindSandbox].Digest == "" {
		return nil, fmt.Errorf("boot index missing required kind %q: %w", KindSandbox, ErrMissingSandbox)
	}
	return components, nil
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
