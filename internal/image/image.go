package image

import (
	"context"
	"fmt"
	"sort"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/errdefs"
	digestpkg "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

// TemplateRecordNamePrefix reserves a local containerd image-record keyspace
// for Template lifecycle management. These names are opaque local keys, not
// registry references.
const TemplateRecordNamePrefix = "io.conch.template/"

// TemplateRecordName deterministically maps a user-visible logical Template
// Name to its internal containerd image-record name.
func TemplateRecordName(logicalName string) string {
	return TemplateRecordNamePrefix + strings.TrimSpace(logicalName)
}

// TemplateNameFromRecordName reverses TemplateRecordName for a well-formed
// internal Template record name.
func TemplateNameFromRecordName(recordName string) (string, bool) {
	if !strings.HasPrefix(recordName, TemplateRecordNamePrefix) {
		return "", false
	}
	name := strings.TrimPrefix(recordName, TemplateRecordNamePrefix)
	if name == "" || strings.TrimSpace(name) != name {
		return "", false
	}
	return name, true
}

func Pull(ctx context.Context, client *containerdclient.Client, req runtimeapi.PullImageOptions) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	if req.ImageName == "" {
		return fmt.Errorf("%w: image_name is required", ErrInvalidArgument)
	}
	if _, ok := TemplateNameFromRecordName(strings.TrimSpace(req.ImageName)); ok {
		return fmt.Errorf("%w: image name %s is reserved for internal Template records", ErrInvalidArgument, req.ImageName)
	}
	pullCtx := containerdclient.NewNamespaceContext(ctx)
	if existing, err := client.ImageService().Get(pullCtx, req.ImageName); err == nil && isBootIndexImageKind(existing.Labels[ImageKindLabel]) {
		return fmt.Errorf("%w: image name %s belongs to a Template; update it with `conch template pull`", ErrInvalidArgument, req.ImageName)
	} else if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("lookup image %s before pull: %w", req.ImageName, err)
	}
	_, _, err := pullRegistryContent(pullCtx, client, RegistryPullOptions{
		Reference: req.ImageName,
		PlainHTTP: req.PlainHTTP,
		Username:  req.Username,
		Password:  req.Password,
	}, false)
	if err != nil {
		return translateRegistryError(err)
	}
	return nil
}

func pullRegistryContent(
	ctx context.Context,
	client *containerdclient.Client,
	req RegistryPullOptions,
	bootIndexOnly bool,
) (images.Image, string, error) {
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.PlainHTTP,
		Credentials: func(string) (string, string, error) {
			return req.Username, req.Password, nil
		},
	})
	var (
		probed bool
		kind   string
	)
	gateRoot := func(next images.Handler) images.Handler {
		return images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			children, err := next.Handle(ctx, desc)
			if err != nil {
				return nil, err
			}
			if probed {
				return children, nil
			}

			// Dispatch visits the root before starting concurrent traversal of
			// its children, so the first descriptor handled here is the target.
			probed = true
			kind, err = DetectImageKind(ctx, client.ContentStore(), desc)
			if err != nil {
				return nil, err
			}
			if err := validatePullKind(req.Reference, kind, bootIndexOnly); err != nil {
				return nil, err
			}
			return children, nil
		})
	}
	fetched, err := client.Fetch(
		ctx,
		req.Reference,
		containerd.WithResolver(resolver),
		containerd.WithImageHandlerWrapper(gateRoot),
	)
	if err != nil {
		return images.Image{}, "", fmt.Errorf("fetch image %s: %w", req.Reference, err)
	}
	if !probed || kind == "" {
		return images.Image{}, "", fmt.Errorf("classify fetched image %s: root descriptor was not inspected", req.Reference)
	}
	if err := SetImageKindLabel(ctx, client.ImageService(), fetched.Name, kind); err != nil {
		return images.Image{}, "", err
	}
	return fetched, kind, nil
}

func validatePullKind(reference, kind string, bootIndexOnly bool) error {
	isBootIndex := kind == ImageKindBootIndexCold || kind == ImageKindBootIndexResume
	if (!bootIndexOnly && kind == ImageKindOCIImage) || (bootIndexOnly && isBootIndex) {
		return nil
	}
	if bootIndexOnly {
		return fmt.Errorf(
			"%w: %s is not a Conch Boot Index; use `conch image pull %s`",
			ErrInvalidArgument,
			reference,
			reference,
		)
	}
	return fmt.Errorf(
		"%w: %s is a Conch Boot Index (%s); use `conch template pull %s`",
		ErrInvalidArgument,
		reference,
		kind,
		reference,
	)
}

func Push(ctx context.Context, client *containerdclient.Client, req runtimeapi.PushImageOptions) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	if req.LocalImage == "" {
		return fmt.Errorf("%w: local_image is required", ErrInvalidArgument)
	}
	if req.RemoteImage == "" {
		return fmt.Errorf("%w: remote_image is required", ErrInvalidArgument)
	}
	if _, ok := TemplateNameFromRecordName(strings.TrimSpace(req.LocalImage)); ok {
		return fmt.Errorf("%w: image name %s is reserved for internal Template records; use `conch template push`", ErrInvalidArgument, req.LocalImage)
	}

	pushCtx := containerdclient.NewNamespaceContext(ctx)
	img, err := client.GetImage(pushCtx, req.LocalImage)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ErrNotFound.Wrap(err)
		}
		return fmt.Errorf("lookup image %s: %w", req.LocalImage, err)
	}
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.PlainHTTP,
		Credentials: func(string) (string, string, error) {
			return req.Username, req.Password, nil
		},
	})
	if err := client.Push(pushCtx, req.RemoteImage, img.Target(), containerd.WithResolver(resolver), containerd.WithMaxConcurrentUploadedLayers(1)); err != nil {
		return translateRegistryError(fmt.Errorf("push image %s -> %s: %w", req.LocalImage, req.RemoteImage, err))
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
		if errdefs.IsInvalidArgument(err) {
			return nil, ErrInvalidArgument.Wrap(err)
		}
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
	_, isTemplateRecord := TemplateNameFromRecordName(name)
	if name == "" || digest == "" || isDigestOnlyRef(name) || isTemplateRecord {
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
		return fmt.Errorf("%w: image_name is required", ErrInvalidArgument)
	}
	if _, ok := TemplateNameFromRecordName(strings.TrimSpace(req.ImageName)); ok {
		return fmt.Errorf("%w: image name %s is reserved for internal Template records; use `conch template rm`", ErrInvalidArgument, req.ImageName)
	}
	removeCtx := containerdclient.NewNamespaceContext(ctx)
	record, err := client.ImageService().Get(removeCtx, req.ImageName)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ErrNotFound.Wrap(err)
		}
		return fmt.Errorf("lookup image %s: %w", req.ImageName, err)
	}
	if isBootIndexImageKind(record.Labels[ImageKindLabel]) {
		return fmt.Errorf("%w: image name %s belongs to a Template; remove it with `conch template rm`", ErrInvalidArgument, req.ImageName)
	}
	opts := []images.DeleteOpt{}
	if req.Synchronous {
		opts = append(opts, images.SynchronousDelete())
	}
	if err := client.ImageService().Delete(removeCtx, req.ImageName, opts...); err != nil {
		if errdefs.IsNotFound(err) {
			return ErrNotFound.Wrap(err)
		}
		return fmt.Errorf("remove image %s: %w", req.ImageName, err)
	}
	return nil
}

func isBootIndexImageKind(kind string) bool {
	return kind == ImageKindBootIndexCold || kind == ImageKindBootIndexResume
}
