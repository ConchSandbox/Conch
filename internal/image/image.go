package image

import (
	"context"
	"fmt"
	"sort"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	digestpkg "github.com/opencontainers/go-digest"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

func Pull(ctx context.Context, client *containerdclient.Client, req runtimeapi.PullImageOptions) (runtimeapi.PullImageResult, error) {
	if client == nil || client.Client == nil {
		return runtimeapi.PullImageResult{}, fmt.Errorf("containerd client is required")
	}
	if req.ImageName == "" {
		return runtimeapi.PullImageResult{}, fmt.Errorf("%w: image_name is required", ErrInvalidRequest)
	}

	pullCtx := containerdclient.NewNamespaceContext(ctx)
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.PlainHTTP,
		Credentials: func(string) (string, string, error) {
			return req.Username, req.Password, nil
		},
	})
	if _, err := client.Pull(pullCtx, req.ImageName, containerd.WithResolver(resolver)); err != nil {
		return runtimeapi.PullImageResult{}, fmt.Errorf("pull image %s: %w", req.ImageName, err)
	}

	fetched, err := client.Fetch(pullCtx, req.ImageName, containerd.WithResolver(resolver))
	if err != nil {
		return runtimeapi.PullImageResult{}, fmt.Errorf("fetch all Conch image content: %w", err)
	}
	kind := DetectImageKind(pullCtx, client.ContentStore(), fetched.Target)
	if err := SetImageKindLabel(pullCtx, client.ImageService(), fetched.Name, kind); err != nil {
		return runtimeapi.PullImageResult{}, err
	}
	if req.SkipUnpack {
		return runtimeapi.PullImageResult{}, nil
	}

	results, err := UnpackAllSubImages(pullCtx, client.Client, req.ImageName)
	if err != nil {
		return runtimeapi.PullImageResult{}, fmt.Errorf("unpack pulled image: %w", err)
	}
	return runtimeapi.PullImageResult{Refs: results}, nil
}

func Push(ctx context.Context, client *containerdclient.Client, req runtimeapi.PushImageOptions) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	if req.LocalImage == "" {
		return fmt.Errorf("%w: local_image is required", ErrInvalidRequest)
	}
	if req.RemoteImage == "" {
		return fmt.Errorf("%w: remote_image is required", ErrInvalidRequest)
	}

	pushCtx := containerdclient.NewNamespaceContext(ctx)
	img, err := client.GetImage(pushCtx, req.LocalImage)
	if err != nil {
		return fmt.Errorf("lookup image %s: %w", req.LocalImage, err)
	}
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.PlainHTTP,
		Credentials: func(string) (string, string, error) {
			return req.Username, req.Password, nil
		},
	})
	if err := client.Push(pushCtx, req.RemoteImage, img.Target(), containerd.WithResolver(resolver), containerd.WithMaxConcurrentUploadedLayers(1)); err != nil {
		return fmt.Errorf("push image %s -> %s: %w", req.LocalImage, req.RemoteImage, err)
	}
	return nil
}

func List(ctx context.Context, client *containerdclient.Client, req runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error) {
	if client == nil || client.Client == nil {
		return nil, fmt.Errorf("containerd client is required")
	}
	listCtx := containerdclient.NewNamespaceContext(ctx)
	items, err := client.ImageService().List(listCtx, req.Filters...)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	out := make([]runtimeapi.ImageRecord, 0, len(items))
	for _, item := range items {
		kind := strings.TrimSpace(item.Labels[ImageKindLabel])
		if kind == "" {
			kind = ImageKindOCIImage
		}
		out = append(out, runtimeapi.ImageRecord{
			Name:            item.Name,
			TargetDigest:    item.Target.Digest.String(),
			RepoDigests:     imageRepoDigests(item.Name, item.Target.Digest.String()),
			TargetMediaType: item.Target.MediaType,
			Size:            item.Target.Size,
			Kind:            kind,
			Labels:          item.Labels,
			CreatedAt:       item.CreatedAt,
			UpdatedAt:       item.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func imageRepoDigests(name, digest string) []string {
	name = strings.TrimSpace(name)
	digest = strings.TrimSpace(digest)
	if name == "" || digest == "" || isDigestOnlyRef(name) {
		return nil
	}
	base := name
	if repo, _, ok := strings.Cut(base, "@"); ok {
		base = repo
	} else {
		lastSlash := strings.LastIndex(base, "/")
		lastColon := strings.LastIndex(base, ":")
		if lastColon > lastSlash {
			base = base[:lastColon]
		}
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return nil
	}
	return []string{base + "@" + digest}
}

func isDigestOnlyRef(ref string) bool {
	if _, err := digestpkg.Parse(ref); err == nil {
		return true
	}
	algo, _, ok := strings.Cut(ref, ":")
	if !ok || strings.Contains(algo, "/") {
		return false
	}
	switch algo {
	case "sha256", "sha384", "sha512":
		return true
	default:
		return false
	}
}

func Remove(ctx context.Context, client *containerdclient.Client, req runtimeapi.RemoveImageOptions) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	if req.ImageName == "" {
		return fmt.Errorf("%w: image_name is required", ErrInvalidRequest)
	}
	removeCtx := containerdclient.NewNamespaceContext(ctx)
	opts := []images.DeleteOpt{}
	if req.Synchronous {
		opts = append(opts, images.SynchronousDelete())
	}
	if err := client.ImageService().Delete(removeCtx, req.ImageName, opts...); err != nil {
		return fmt.Errorf("remove image %s: %w", req.ImageName, err)
	}
	return nil
}
