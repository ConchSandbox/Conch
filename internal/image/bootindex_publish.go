package image

import (
	"context"
	"fmt"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

func PublishBootIndex(ctx context.Context, client *containerdclient.Client, req PublishBootIndexOptions) (PublishBootIndexResult, error) {
	if client == nil || client.Client == nil {
		return PublishBootIndexResult{}, fmt.Errorf("containerd client is required")
	}
	if req.RootfsImageName == "" {
		return PublishBootIndexResult{}, fmt.Errorf("%w: rootfs_image_name is required", ErrInvalidRequest)
	}
	if req.KernelPath == "" {
		return PublishBootIndexResult{}, fmt.Errorf("%w: kernel_path is required", ErrInvalidRequest)
	}
	if req.InitrdPath == "" {
		return PublishBootIndexResult{}, fmt.Errorf("%w: initrd_path is required", ErrInvalidRequest)
	}
	if req.BootIndexTag == "" {
		return PublishBootIndexResult{}, fmt.Errorf("%w: boot_index_tag is required", ErrInvalidRequest)
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
		Tag:              req.BootIndexTag,
	})
	if err != nil {
		return PublishBootIndexResult{}, fmt.Errorf("build boot index content: %w", err)
	}

	if err := publishBootIndexRecord(publishCtx, client, req.BootIndexTag, indexDesc, ImageKindBootIndexCold); err != nil {
		return PublishBootIndexResult{}, err
	}

	return PublishBootIndexResult{
		BootIndexDigest: indexDesc.Digest.String(),
		ImageName:       req.BootIndexTag,
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
		return fmt.Errorf("%w: boot_index_digest is required", ErrInvalidRequest)
	}
	req.RemoteReference = strings.TrimSpace(req.RemoteReference)
	if req.RemoteReference == "" {
		return fmt.Errorf("%w: remote_reference is required", ErrInvalidRequest)
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
		return fmt.Errorf("push boot index %s -> %s: %w", desc.Digest, req.RemoteReference, err)
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
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: source_boot_index_digest is required", ErrInvalidRequest)
	}
	req.BootIndexTag = strings.TrimSpace(req.BootIndexTag)
	if req.BootIndexTag == "" {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: boot_index_tag is required", ErrInvalidRequest)
	}
	req.MemRoot = strings.TrimSpace(req.MemRoot)
	if req.MemRoot == "" {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: mem_root is required", ErrInvalidRequest)
	}
	req.VMMName = strings.TrimSpace(req.VMMName)
	if req.VMMName == "" {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: vmm_name is required", ErrInvalidRequest)
	}
	if req.MemorySizeMB <= 0 {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("%w: memory_size_mb must be positive", ErrInvalidRequest)
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

	memDesc, err := BuildNativeComponentInContent(publishCtx, client.ContentStore(), []string{req.MemRoot}, KindMemSnapshot, req.BootIndexTag+"-mem")
	if err != nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("publish captured mem component: %w", err)
	}
	indexDesc, err := BuildBootIndexInContent(publishCtx, client.ContentStore(), BootIndexContentOptions{
		RootfsDescriptor:  sourceInfo.RootfsDescriptor,
		MemDescriptor:     memDesc,
		SandboxDescriptor: sourceInfo.SandboxDescriptor,
		Tag:               req.BootIndexTag,
		VMMName:           req.VMMName,
		MemorySizeMB:      req.MemorySizeMB,
	})
	if err != nil {
		return PublishCheckpointBootIndexResult{}, fmt.Errorf("build checkpoint boot index: %w", err)
	}
	if err := publishBootIndexRecord(publishCtx, client, req.BootIndexTag, indexDesc, ImageKindBootIndexResume); err != nil {
		return PublishCheckpointBootIndexResult{}, err
	}
	return PublishCheckpointBootIndexResult{
		BootIndexDigest: indexDesc.Digest.String(),
		ImageName:       req.BootIndexTag,
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
			return fmt.Errorf("create boot image record %s: %w", tag, err)
		}
	}
	return nil
}
