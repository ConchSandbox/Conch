package image

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/id"
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
	probed := false
	gateRoot := func(next images.Handler) images.Handler {
		return images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			children, err := next.Handle(ctx, desc)
			if err != nil {
				return nil, err
			}
			if probed {
				return children, nil
			}
			probed = true
			kind, err := DetectImageKind(ctx, client.ContentStore(), desc)
			if err != nil {
				return nil, err
			}
			if err := validatePullKind(req.Reference, kind, true); err != nil {
				return nil, err
			}
			return children, nil
		})
	}
	fetched, err := client.Fetch(
		pullCtx,
		req.Reference,
		containerd.WithResolver(namedResolver),
		containerd.WithImageHandlerWrapper(gateRoot),
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
	if !probed || namedResolver.resolvedName == "" {
		return fmt.Errorf("classify fetched template %s: root descriptor was not inspected", req.Reference)
	}
	info, err := InspectBootIndexContent(pullCtx, client.ContentStore(), fetched.Target)
	if err != nil {
		return fmt.Errorf("validate pulled Boot Index %s: %w", namedResolver.resolvedName, err)
	}
	return consume(pullCtx, PulledBootIndex{
		Info:            info,
		SourceImageName: namedResolver.resolvedName,
		Target:          fetched.Target,
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
