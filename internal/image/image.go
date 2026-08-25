package image

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/errdefs"
	digestpkg "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

func Pull(ctx context.Context, client *containerdclient.Client, req runtimeapi.PullImageOptions) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	if req.ImageName == "" {
		return fmt.Errorf("%w: image_name is required", ErrInvalidArgument)
	}
	if IsCanonicalTemplateRef(req.ImageName) {
		return fmt.Errorf("%w: image name %s is reserved for Template lifecycle management", ErrInvalidArgument, req.ImageName)
	}

	pullCtx := containerdclient.NewNamespaceContext(ctx)
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

// PullBootIndex fetches a registry Boot Index and validates its complete
// descriptor closure without unpacking component snapshots. The top-level
// index is classified before child descriptors are fetched, so ordinary OCI
// images are rejected without downloading their configs or layers.
func PullBootIndex(ctx context.Context, client *containerdclient.Client, req RegistryPullOptions) (PullBootIndexResult, error) {
	if client == nil || client.Client == nil {
		return PullBootIndexResult{}, fmt.Errorf("containerd client is required")
	}
	if strings.TrimSpace(req.Reference) == "" {
		return PullBootIndexResult{}, fmt.Errorf("%w: reference is required", ErrInvalidArgument)
	}
	if IsCanonicalTemplateRef(req.Reference) {
		return PullBootIndexResult{}, fmt.Errorf("%w: registry reference %s is reserved for local Template lifecycle management", ErrInvalidArgument, req.Reference)
	}
	if req.PreferLazy {
		result, supported, err := pullLazyBootIndex(containerdclient.NewNamespaceContext(ctx), client, req)
		if err != nil {
			return PullBootIndexResult{}, translateRegistryError(err)
		}
		if supported {
			return result, nil
		}
	}

	pullCtx := containerdclient.NewNamespaceContext(ctx)
	fetched, _, err := pullRegistryContent(pullCtx, client, req, true)
	if err != nil {
		return PullBootIndexResult{}, translateRegistryError(err)
	}
	info, err := InspectBootIndexContent(pullCtx, client.ContentStore(), fetched.Target)
	if err != nil {
		return PullBootIndexResult{}, errors.Join(
			fmt.Errorf("validate pulled Boot Index %s: %w", fetched.Name, err),
			RemoveFetchedImageRecord(ctx, client.ImageService(), fetched.Name, fetched.Target),
		)
	}
	buildRef, err := CanonicalTemplateRef(fetched.Target.Digest.String())
	if err != nil {
		return PullBootIndexResult{}, errors.Join(
			err,
			RemoveFetchedImageRecord(ctx, client.ImageService(), fetched.Name, fetched.Target),
		)
	}
	return PullBootIndexResult{
		Info:            info,
		BuildRef:        buildRef,
		SourceImageName: fetched.Name,
		Target:          fetched.Target,
	}, nil
}

// RemoveFetchedImageRecord releases the temporary image root without inheriting request cancellation.
func RemoveFetchedImageRecord(ctx context.Context, store images.Store, name string, target ocispec.Descriptor) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := store.Delete(
		containerdclient.NewNamespaceContext(cleanupCtx),
		name,
		images.DeleteTarget(&target),
	); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove fetched image record %s: %w", name, err)
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
		return fmt.Errorf("%w: image_name is required", ErrInvalidArgument)
	}
	if IsCanonicalTemplateRef(req.ImageName) {
		return fmt.Errorf("%w: canonical Template image %s must be removed with `conch template rm`", ErrInvalidArgument, req.ImageName)
	}
	removeCtx := containerdclient.NewNamespaceContext(ctx)
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
