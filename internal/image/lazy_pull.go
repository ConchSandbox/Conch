package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

type LazyMemoryMetadata struct {
	Layer      ocispec.Descriptor
	FileOffset int64
	FileSize   int64
	Profile    []byte
}

func pullLazyBootIndex(ctx context.Context, client *containerdclient.Client, req RegistryPullOptions) (PullBootIndexResult, bool, error) {
	if strings.TrimSpace(req.Username) != "" || strings.TrimSpace(req.Password) != "" {
		return PullBootIndexResult{}, false, nil
	}
	resolver := docker.NewResolver(docker.ResolverOptions{PlainHTTP: req.PlainHTTP})
	name, root, err := resolver.Resolve(ctx, req.Reference)
	if err != nil {
		return PullBootIndexResult{}, false, fmt.Errorf("resolve lazy boot index %s: %w", req.Reference, err)
	}
	if root.MediaType != ocispec.MediaTypeImageIndex {
		return PullBootIndexResult{}, false, nil
	}
	fetcher, err := resolver.Fetcher(ctx, name)
	if err != nil {
		return PullBootIndexResult{}, false, err
	}
	store := client.ContentStore()
	if err := fetchDescriptor(ctx, store, fetcher, root); err != nil {
		return PullBootIndexResult{}, false, err
	}
	raw, err := content.ReadBlob(ctx, store, root)
	if err != nil {
		return PullBootIndexResult{}, false, err
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return PullBootIndexResult{}, false, err
	}
	info, err := inspectBootIndexMetadata(root, index)
	if err != nil {
		return PullBootIndexResult{}, false, ErrInvalidContent.Wrap(err)
	}
	if !info.Resume {
		return PullBootIndexResult{}, false, nil
	}

	if err := fetchDescriptors(ctx, index.Manifests, func(ctx context.Context, desc ocispec.Descriptor) error {
		return fetchDescriptor(ctx, store, fetcher, desc)
	}); err != nil {
		return PullBootIndexResult{}, false, err
	}

	var contentDescriptors []ocispec.Descriptor
	for _, component := range index.Manifests {
		manifest, err := readManifest(ctx, store, component)
		if err != nil {
			return PullBootIndexResult{}, false, err
		}
		contentDescriptors = append(contentDescriptors, manifest.Config)
		if getKind(component) == KindMemSnapshot {
			if _, err := parseLazyMemoryMetadata(info.PreGateProfile, manifest); err != nil {
				return PullBootIndexResult{}, false, nil
			}
			continue
		}
		contentDescriptors = append(contentDescriptors, manifest.Layers...)
	}
	if err := fetchDescriptors(ctx, contentDescriptors, func(ctx context.Context, desc ocispec.Descriptor) error {
		return fetchDescriptor(ctx, store, fetcher, desc)
	}); err != nil {
		return PullBootIndexResult{}, false, err
	}
	buildRef, err := CanonicalTemplateRef(root.Digest.String())
	if err != nil {
		return PullBootIndexResult{}, false, err
	}
	return PullBootIndexResult{Info: info, BuildRef: buildRef, Target: root, Lazy: true}, true, nil
}

func fetchDescriptor(ctx context.Context, store content.Store, fetcher remotes.Fetcher, desc ocispec.Descriptor) error {
	if _, err := store.Info(ctx, desc.Digest); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	if err := remotes.Fetch(ctx, store, fetcher, desc); err != nil && !errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("fetch descriptor %s: %w", desc.Digest, err)
	}
	return nil
}

func fetchDescriptors(ctx context.Context, descriptors []ocispec.Descriptor, fetch func(context.Context, ocispec.Descriptor) error) error {
	group, groupCtx := errgroup.WithContext(ctx)
	seen := make(map[string]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		key := descriptor.Digest.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		descriptor := descriptor
		group.Go(func() error {
			return fetch(groupCtx, descriptor)
		})
	}
	return group.Wait()
}

func readManifest(ctx context.Context, store content.Store, desc ocispec.Descriptor) (ocispec.Manifest, error) {
	raw, err := content.ReadBlob(ctx, store, desc)
	if err != nil {
		return ocispec.Manifest{}, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return ocispec.Manifest{}, err
	}
	return manifest, nil
}

func parseLazyMemoryMetadata(encodedProfile string, manifest ocispec.Manifest) (LazyMemoryMetadata, error) {
	if len(manifest.Layers) != 1 {
		return LazyMemoryMetadata{}, fmt.Errorf("lazy memory component requires exactly one EROFS layer")
	}
	layer := manifest.Layers[0]
	offset, err := strconv.ParseInt(strings.TrimSpace(layer.Annotations[AnnotationMemoryFileOffset]), 10, 64)
	if err != nil || offset <= 0 {
		return LazyMemoryMetadata{}, fmt.Errorf("lazy memory layer has invalid file offset")
	}
	fileSize, err := strconv.ParseInt(strings.TrimSpace(layer.Annotations[AnnotationMemoryFileSize]), 10, 64)
	if err != nil || fileSize <= 0 || offset+fileSize > layer.Size {
		return LazyMemoryMetadata{}, fmt.Errorf("lazy memory layer has invalid file size")
	}
	metadata := LazyMemoryMetadata{Layer: layer, FileOffset: offset, FileSize: fileSize}
	if strings.TrimSpace(encodedProfile) == "" {
		// A profile is optional: the first restore without one pays the full
		// sequential read cost and learns it.
		return metadata, nil
	}
	profile, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encodedProfile))
	if err != nil {
		return LazyMemoryMetadata{}, fmt.Errorf("decode lazy memory profile: %w", err)
	}
	var identity struct {
		Version  int      `json:"version"`
		PageSize int64    `json:"page_size"`
		FileSize int64    `json:"file_size"`
		Offsets  []uint64 `json:"offsets"`
	}
	if err := json.Unmarshal(profile, &identity); err != nil {
		return LazyMemoryMetadata{}, err
	}
	if identity.Version != 1 || identity.PageSize <= 0 || identity.FileSize != fileSize || len(identity.Offsets) == 0 {
		return LazyMemoryMetadata{}, fmt.Errorf("lazy memory profile identity does not match EROFS memory file")
	}
	for _, pageOffset := range identity.Offsets {
		if pageOffset >= uint64(fileSize) || pageOffset%uint64(identity.PageSize) != 0 {
			return LazyMemoryMetadata{}, fmt.Errorf("lazy memory profile contains invalid offset %d", pageOffset)
		}
	}
	metadata.Profile = profile
	return metadata, nil
}

func lazyMemoryMetadata(ctx context.Context, store content.Store, info BootIndexInfo) (LazyMemoryMetadata, error) {
	manifest, err := readManifest(ctx, store, info.MemDescriptor)
	if err != nil {
		return LazyMemoryMetadata{}, err
	}
	return parseLazyMemoryMetadata(info.PreGateProfile, manifest)
}
