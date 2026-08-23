package image

import (
	"context"
	"fmt"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/opencontainers/go-digest"

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
	rootfsImage, err := client.ImageService().Get(ctx, req.RootfsImageName)
	if err != nil {
		return PublishBootIndexResult{}, fmt.Errorf("lookup rootfs image %s: %w", req.RootfsImageName, err)
	}
	indexDesc, err := BuildBootIndexInContent(ctx, client.ContentStore(), BootIndexContentOptions{
		RootfsDescriptor: rootfsImage.Target,
		KernelPath:       req.KernelPath,
		InitrdPath:       req.InitrdPath,
	})
	if err != nil {
		return PublishBootIndexResult{}, fmt.Errorf("build boot index content: %w", err)
	}

	buildRef, err := CanonicalTemplateRef(indexDesc.Digest.String())
	if err != nil {
		return PublishBootIndexResult{}, err
	}

	return PublishBootIndexResult{
		BootIndexDigest: indexDesc.Digest.String(),
		BuildRef:        buildRef,
		Target:          indexDesc,
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
// The current implementation takes a VMM-specific MemRoot staging directory as
// its mutable checkpoint input. A future, more containerd-native implementation
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

	_, sourceInfo, err := inspectBootIndexByDigest(ctx, client.ContentStore(), req.SourceBootIndexDigest)
	if err != nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("inspect source boot index: %w", err)
	}
	if sourceInfo.VMMName != "" && sourceInfo.VMMName != req.VMMName {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("source boot index VMM %q does not match capture VMM %q", sourceInfo.VMMName, req.VMMName)
	}

	memDesc, err := BuildNativeComponentInContent(ctx, client.ContentStore(), []string{req.MemRoot}, KindMemSnapshot)
	if err != nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("publish captured mem component: %w", err)
	}
	indexDesc, err := BuildBootIndexInContent(ctx, client.ContentStore(), BootIndexContentOptions{
		RootfsDescriptor:  sourceInfo.RootfsDescriptor,
		MemDescriptor:     memDesc,
		SandboxDescriptor: sourceInfo.SandboxDescriptor,
		VMMName:           req.VMMName,
		MemorySizeMB:      req.MemorySizeMB,
	})
	if err != nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("build checkpoint boot index: %w", err)
	}
	buildRef, err := CanonicalTemplateRef(indexDesc.Digest.String())
	if err != nil {
		return PublishCheckpointBootIndexResult{}, err
	}
	return PublishCheckpointBootIndexResult{
		BootIndexDigest: indexDesc.Digest.String(),
		BuildRef:        buildRef,
		Target:          indexDesc,
	}, nil
}
