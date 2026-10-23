package image

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/id"
	"github.com/openeuler/Conch/internal/platformutil"
	"github.com/openeuler/Conch/pkg/ulog"
)

// WithPulledBootIndex fetches and validates a registry Boot Index, then invokes
// consume while the fetched content remains leased. Temporary image-record and
// lease lifecycle details do not escape this function.
func WithPulledBootIndex(
	ctx context.Context,
	client *containerdclient.Client,
	req RegistryPullOptions,
	consume func(context.Context, PulledBootIndex) error,
) (retErr error) {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	if strings.TrimSpace(req.Reference) == "" {
		return fmt.Errorf("%w: reference is required", ErrInvalidArgument)
	}
	if consume == nil {
		return fmt.Errorf("Boot Index consumer is required")
	}
	platform, err := platformutil.Parse(req.Platform)
	if err != nil {
		return ErrInvalidArgument.Wrap(err)
	}
	pullCtx, done, err := client.WithLease(containerdclient.NewNamespaceContext(ctx))
	if err != nil {
		return fmt.Errorf("create Boot Index pull lease: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(
			containerdclient.NewNamespaceContext(context.WithoutCancel(ctx)),
			10*time.Second,
		)
		defer cancel()
		if err := done(cleanupCtx); err != nil {
			ulog.GetLogger().Warn("failed to remove Boot Index pull lease", ulog.F("error", err))
		}
	}()
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.PlainHTTP,
		Credentials: func(string) (string, string, error) {
			return req.Username, req.Password, nil
		},
	})
	temporaryID, err := id.New()
	if err != nil {
		return err
	}
	temporaryName := "localhost/conch/template-fetch:" + temporaryID
	namedResolver := &temporaryImageResolver{Resolver: resolver, temporaryName: temporaryName}
	selection := bootIndexPullSelection{store: client.ContentStore(), reference: req.Reference, platform: platform}
	fetched, err := client.Fetch(
		pullCtx,
		req.Reference,
		containerd.WithResolver(namedResolver),
		// A Boot Index contains several components of ONE platform. All of
		// those components must be fetched; only an outer platform index is
		// filtered by the selection wrapper.
		containerd.WithPlatformMatcher(platforms.All),
		containerd.WithImageHandlerWrapper(selection.wrap),
		containerd.WithPullLabels(map[string]string{
			"containerd.io/gc.expire": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}),
	)
	if err != nil {
		return translateRegistryError(err)
	}
	defer func() {
		cleanupErr := removeTemporaryImageRecord(ctx, client.ImageService(), fetched.Name, fetched.Target)
		if cleanupErr == nil {
			return
		}
		if retErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
			return
		}
		ulog.GetLogger().Warn("failed to remove temporary Boot Index pull image record",
			ulog.F("image", fetched.Name),
			ulog.F("error", cleanupErr))
	}()
	if selection.target.Digest == "" || namedResolver.resolvedName == "" {
		return fmt.Errorf("classify fetched template %s: root descriptor was not inspected", req.Reference)
	}
	info, err := InspectBootIndexContent(pullCtx, client.ContentStore(), selection.target)
	if err != nil {
		return fmt.Errorf("validate pulled Boot Index %s: %w", namedResolver.resolvedName, err)
	}
	if info.Platform != platforms.Format(platform) {
		return ErrInvalidArgument.Wrap(fmt.Errorf("template platform %s does not match requested %s", info.Platform, platforms.Format(platform)))
	}
	return consume(pullCtx, PulledBootIndex{
		Info:            info,
		SourceImageName: namedResolver.resolvedName,
		Target:          selection.target,
	})
}

// Dispatch handles each parent before starting its children. Until a Boot
// Index has been selected, this wrapper returns at most one child, so selection
// state is finalized before any concurrent component traversal begins.
type bootIndexPullSelection struct {
	store      content.Store
	reference  string
	platform   ocispec.Platform
	outerIndex bool
	target     ocispec.Descriptor
}

func (s *bootIndexPullSelection) wrap(next images.Handler) images.Handler {
	return images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		children, err := next.Handle(ctx, desc)
		if err != nil {
			return nil, err
		}
		if s.target.Digest != "" {
			return children, nil
		}
		kind, err := DetectImageKind(ctx, s.store, desc)
		if err != nil {
			return nil, err
		}
		if isBootIndexImageKind(kind) {
			for _, child := range children {
				if child.Platform != nil && !platforms.OnlyStrict(s.platform).Match(*child.Platform) {
					return nil, ErrInvalidArgument.Wrap(fmt.Errorf("template component platform %s does not match requested %s", platforms.Format(*child.Platform), platforms.Format(s.platform)))
				}
			}
			s.target = desc
			return children, nil
		}
		if !s.outerIndex && images.IsIndexType(desc.MediaType) {
			var selected []ocispec.Descriptor
			for _, child := range children {
				if child.MediaType == ocispec.MediaTypeImageIndex && child.Platform != nil && platforms.OnlyStrict(s.platform).Match(*child.Platform) {
					selected = append(selected, child)
				}
			}
			if len(selected) != 1 {
				return nil, ErrInvalidArgument.Wrap(fmt.Errorf("%s: expected one Boot Index for platform %s, found %d", s.reference, platforms.Format(s.platform), len(selected)))
			}
			s.outerIndex = true
			return selected, nil
		}
		return nil, validatePullKind(s.reference, kind, true)
	})
}

type temporaryImageResolver struct {
	remotes.Resolver
	temporaryName string
	resolvedName  string
}

func (r *temporaryImageResolver) Resolve(ctx context.Context, ref string) (string, ocispec.Descriptor, error) {
	name, target, err := r.Resolver.Resolve(ctx, ref)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}
	r.resolvedName = name
	return r.temporaryName, target, nil
}

func (r *temporaryImageResolver) Fetcher(ctx context.Context, ref string) (remotes.Fetcher, error) {
	if ref != r.temporaryName || r.resolvedName == "" {
		return nil, fmt.Errorf("unexpected temporary image reference %q", ref)
	}
	return r.Resolver.Fetcher(ctx, r.resolvedName)
}

// removeTemporaryImageRecord releases a successful Client.Fetch record
// without inheriting request cancellation. Its target condition prevents a
// cleanup race from deleting an unrelated record.
func removeTemporaryImageRecord(ctx context.Context, store images.Store, name string, target ocispec.Descriptor) error {
	cleanupCtx, cancel := context.WithTimeout(
		containerdclient.NewNamespaceContext(context.WithoutCancel(ctx)),
		10*time.Second,
	)
	defer cancel()
	if err := store.Delete(cleanupCtx, name, images.DeleteTarget(&target)); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove temporary image record %s: %w", name, err)
	}
	return nil
}
