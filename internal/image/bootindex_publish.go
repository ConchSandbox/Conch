package image

import (
	"context"
	"fmt"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

const canonicalTemplateRepository = "localhost/conch/template"

// CanonicalTemplateRef returns the sole local image-record name owned by a
// Template. Template identity is the immutable Boot Index digest, so this
// mapping must remain deterministic and injective.
func CanonicalTemplateRef(rawDigest string) (string, error) {
	parsed, err := digest.Parse(strings.TrimSpace(rawDigest))
	if err != nil {
		return "", fmt.Errorf("%w: invalid boot index digest %q: %v", ErrInvalidArgument, rawDigest, err)
	}
	return canonicalTemplateRepository + ":" + parsed.Algorithm().String() + "-" + parsed.Encoded(), nil
}

// IsCanonicalTemplateRef reports whether ref belongs to the digest-derived
// image-record namespace reserved for Template lifecycle management.
func IsCanonicalTemplateRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	prefix := canonicalTemplateRepository + ":"
	if !strings.HasPrefix(ref, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(ref, prefix)
	separator := strings.IndexByte(suffix, '-')
	if separator <= 0 || separator == len(suffix)-1 {
		return false
	}
	canonical, err := CanonicalTemplateRef(suffix[:separator] + ":" + suffix[separator+1:])
	return err == nil && canonical == ref
}

// EnsureCanonicalBootIndexRecord creates or validates the canonical local
// image record for target. Content is not copied; the record is another GC
// root for the same immutable descriptor closure.
func EnsureCanonicalBootIndexRecord(
	ctx context.Context,
	client *containerdclient.Client,
	target ocispec.Descriptor,
	kind string,
) (string, error) {
	if client == nil || client.Client == nil {
		return "", fmt.Errorf("containerd client is required")
	}
	if target.Digest == "" {
		return "", fmt.Errorf("%w: boot index target digest is required", ErrInvalidArgument)
	}
	if kind != ImageKindBootIndexCold && kind != ImageKindBootIndexResume {
		return "", fmt.Errorf("%w: invalid boot index image kind %q", ErrInvalidArgument, kind)
	}
	name, err := CanonicalTemplateRef(target.Digest.String())
	if err != nil {
		return "", err
	}
	namespaceCtx := containerdclient.NewNamespaceContext(ctx)
	existing, err := client.ImageService().Get(namespaceCtx, name)
	if err == nil && existing.Target.Digest != target.Digest {
		return "", fmt.Errorf("canonical template image %s targets %s, want %s", name, existing.Target.Digest, target.Digest)
	}
	if err != nil && !errdefs.IsNotFound(err) {
		return "", fmt.Errorf("lookup canonical template image %s: %w", name, err)
	}
	if err := publishBootIndexRecord(namespaceCtx, client, name, target, kind); err != nil {
		return "", err
	}
	return name, nil
}

// RemoveCanonicalBootIndexRecord removes only the image metadata root. The
// embedded containerd GC decides when the descriptor closure can be reclaimed.
func RemoveCanonicalBootIndexRecord(ctx context.Context, client *containerdclient.Client, rawDigest string) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	parsed, err := digest.Parse(strings.TrimSpace(rawDigest))
	if err != nil {
		return fmt.Errorf("%w: invalid boot index digest %q: %v", ErrInvalidArgument, rawDigest, err)
	}
	name, err := CanonicalTemplateRef(parsed.String())
	if err != nil {
		return err
	}
	namespaceCtx := containerdclient.NewNamespaceContext(ctx)
	record, err := client.ImageService().Get(namespaceCtx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("lookup canonical template image %s: %w", name, err)
	}
	if record.Target.Digest != parsed {
		return fmt.Errorf("canonical template image %s targets %s, want %s", name, record.Target.Digest, parsed)
	}
	if err := client.ImageService().Delete(namespaceCtx, name, images.DeleteTarget(&record.Target)); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove canonical template image %s: %w", name, err)
	}
	return nil
}

func PublishBootIndex(ctx context.Context, client *containerdclient.Client, req PublishBootIndexOptions) (PublishBootIndexResult, error) {
	if client == nil || client.Client == nil {
		return PublishBootIndexResult{}, fmt.Errorf("containerd client is required")
	}
	if req.RootfsImageName == "" {
		return PublishBootIndexResult{}, fmt.Errorf("%w: rootfs_image_name is required", ErrInvalidArgument)
	}
	if req.KernelPath == "" {
		return PublishBootIndexResult{}, fmt.Errorf("%w: kernel_path is required", ErrInvalidArgument)
	}
	if req.InitrdPath == "" {
		return PublishBootIndexResult{}, fmt.Errorf("%w: initrd_path is required", ErrInvalidArgument)
	}
	namespaceCtx := containerdclient.NewNamespaceContext(ctx)
	publishCtx, done, err := client.WithLease(namespaceCtx)
	if err != nil {
		return PublishBootIndexResult{}, fmt.Errorf("create content lease: %w", err)
	}
	defer done(publishCtx)

	rootfsImage, err := client.ImageService().Get(publishCtx, req.RootfsImageName)
	if err != nil {
		return PublishBootIndexResult{}, fmt.Errorf("lookup rootfs image %s: %w", req.RootfsImageName, err)
	}
	indexDesc, err := BuildBootIndexInContent(publishCtx, client.ContentStore(), BootIndexContentOptions{
		RootfsDescriptor: rootfsImage.Target,
		KernelPath:       req.KernelPath,
		InitrdPath:       req.InitrdPath,
	})
	if err != nil {
		return PublishBootIndexResult{}, fmt.Errorf("build boot index content: %w", err)
	}

	buildRef, err := EnsureCanonicalBootIndexRecord(publishCtx, client, indexDesc, ImageKindBootIndexCold)
	if err != nil {
		return PublishBootIndexResult{}, err
	}

	return PublishBootIndexResult{
		BootIndexDigest: indexDesc.Digest.String(),
		BuildRef:        buildRef,
	}, nil
}

// PushBootIndex pushes the exact descriptor closure selected by an immutable
// digest. Unlike a regular image push, it does not resolve through a mutable
// local image name.
func PushBootIndex(ctx context.Context, client *containerdclient.Client, req PushBootIndexOptions) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	if strings.TrimSpace(req.BootIndexDigest) == "" {
		return fmt.Errorf("%w: boot_index_digest is required", ErrInvalidArgument)
	}
	req.RemoteReference = strings.TrimSpace(req.RemoteReference)
	if req.RemoteReference == "" {
		return fmt.Errorf("%w: remote_reference is required", ErrInvalidArgument)
	}
	pushCtx := containerdclient.NewNamespaceContext(ctx)
	desc, _, err := inspectBootIndexByDigest(pushCtx, client.ContentStore(), req.BootIndexDigest)
	if err != nil {
		return fmt.Errorf("validate boot index %s before push: %w", req.BootIndexDigest, err)
	}
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.PlainHTTP,
		Credentials: func(string) (string, string, error) {
			return req.Username, req.Password, nil
		},
	})
	if err := client.Push(pushCtx, req.RemoteReference, desc, containerd.WithResolver(resolver), containerd.WithMaxConcurrentUploadedLayers(1)); err != nil {
		return translateRegistryError(fmt.Errorf("push boot index %s -> %s: %w", desc.Digest, req.RemoteReference, err))
	}
	return nil
}

// PublishCheckpointBootIndex packages captured memory and VMM state into OCI
// content, reuses the source Boot Index's immutable rootfs and sandbox
// components, and publishes a new Boot Index. It intentionally does not unpack
// the index: checkpoint publication may add content and metadata, but it must
// not create checkpoint snapshots.
//
// The current implementation takes a self-contained MemRoot staging directory
// as its mutable checkpoint input. A future, more containerd-native implementation
// should integrate checkpoint publication with containerd's snapshot commit
// mechanism and publish the committed snapshot as the memory component.
func PublishCheckpointBootIndex(
	ctx context.Context,
	client *containerdclient.Client,
	req PublishCheckpointBootIndexOptions,
) (PublishCheckpointBootIndexResult, error) {
	if client == nil || client.Client == nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("containerd client is required")
	}
	if strings.TrimSpace(req.SourceBootIndexDigest) == "" {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: source_boot_index_digest is required", ErrInvalidArgument)
	}
	req.MemRoot = strings.TrimSpace(req.MemRoot)
	if req.MemRoot == "" {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: mem_root is required", ErrInvalidArgument)
	}
	req.VMMName = strings.TrimSpace(req.VMMName)
	if req.VMMName == "" {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: vmm_name is required", ErrInvalidArgument)
	}
	if req.MemorySizeMB <= 0 {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: memory_size_mb must be positive", ErrInvalidArgument)
	}
	if req.VMMName == "stratovirt" {
		if req.MemoryFormat != MemoryFormatFull && req.MemoryFormat != MemoryFormatIncrementalV1 {
			return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: invalid %s value %q", ErrInvalidArgument, AnnotationMemoryFormat, req.MemoryFormat)
		}
	} else if req.MemoryFormat != "" {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: non-stratovirt checkpoint cannot set %s", ErrInvalidArgument, AnnotationMemoryFormat)
	}

	namespaceCtx := containerdclient.NewNamespaceContext(ctx)
	publishCtx, done, err := client.WithLease(namespaceCtx)
	if err != nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("create content lease: %w", err)
	}
	defer done(publishCtx)

	_, sourceInfo, err := inspectBootIndexByDigest(publishCtx, client.ContentStore(), req.SourceBootIndexDigest)
	if err != nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("inspect source boot index: %w", err)
	}
	if sourceInfo.VMMName != "" && sourceInfo.VMMName != req.VMMName {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("source boot index VMM %q does not match capture VMM %q", sourceInfo.VMMName, req.VMMName)
	}

	var memDesc ocispec.Descriptor
	if req.MemoryFormat == MemoryFormatIncrementalV1 {
		if sourceInfo.Resume && sourceInfo.MemoryFormat != MemoryFormatIncrementalV1 {
			return PublishCheckpointBootIndexResult{}, fmt.Errorf(
				"source memory format %q cannot be appended as %s",
				sourceInfo.MemoryFormat,
				MemoryFormatIncrementalV1,
			)
		}
		memDesc, err = BuildIncrementalMemoryComponentInContent(
			publishCtx,
			client.ContentStore(),
			sourceInfo.MemDescriptor,
			req.MemRoot,
		)
	} else {
		memDesc, err = BuildNativeComponentInContent(publishCtx, client.ContentStore(), []string{req.MemRoot}, KindMemSnapshot)
	}
	if err != nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("publish captured mem component: %w", err)
	}
	indexDesc, err := BuildBootIndexInContent(publishCtx, client.ContentStore(), BootIndexContentOptions{
		RootfsDescriptor:  sourceInfo.RootfsDescriptor,
		MemDescriptor:     memDesc,
		SandboxDescriptor: sourceInfo.SandboxDescriptor,
		VMMName:           req.VMMName,
		MemorySizeMB:      req.MemorySizeMB,
		MemoryFormat:      req.MemoryFormat,
	})
	if err != nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("build checkpoint boot index: %w", err)
	}
	buildRef, err := EnsureCanonicalBootIndexRecord(publishCtx, client, indexDesc, ImageKindBootIndexResume)
	if err != nil {
		return PublishCheckpointBootIndexResult{}, err
	}
	return PublishCheckpointBootIndexResult{
		BootIndexDigest: indexDesc.Digest.String(),
		BuildRef:        buildRef,
	}, nil
}

func publishBootIndexRecord(ctx context.Context, client *containerdclient.Client, tag string, indexDesc ocispec.Descriptor, kind string) error {
	labelHandler := images.SetChildrenLabels(client.ContentStore(), images.ChildrenHandler(client.ContentStore()))
	if err := images.WalkNotEmpty(ctx, labelHandler, indexDesc); err != nil {
		return fmt.Errorf("label boot index content: %w", err)
	}
	imageRecord := images.Image{
		Name:   tag,
		Target: indexDesc,
		Labels: map[string]string{
			ImageKindLabel: kind,
		},
	}
	if _, err := client.ImageService().Update(ctx, imageRecord, "target", "labels."+ImageKindLabel); err != nil {
		if !errdefs.IsNotFound(err) {
			return fmt.Errorf("update boot image record %s: %w", tag, err)
		}
		if _, err := client.ImageService().Create(ctx, imageRecord); err != nil {
			if errdefs.IsAlreadyExists(err) {
				return ErrAlreadyExists.Wrap(err)
			}
			return fmt.Errorf("create boot image record %s: %w", tag, err)
		}
	}
	return nil
}
