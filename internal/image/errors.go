package image

import (
	"context"
	"errors"
	"net"
	"net/http"

	remoteerrors "github.com/containerd/containerd/v2/core/remotes/errors"
	containerdreference "github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/errdefs"
	"github.com/openeuler/Conch/internal/apperror"
)

var (
	ErrInvalidArgument  = apperror.Define(apperror.InvalidArgument, "image.invalid_argument", "invalid image argument")
	ErrInvalidContent   = apperror.Define(apperror.InvalidArgument, "image.invalid_content", "invalid image content")
	ErrNotFound         = apperror.Define(apperror.NotFound, "image.not_found", "image not found")
	ErrAlreadyExists    = apperror.Define(apperror.AlreadyExists, "image.already_exists", "image already exists")
	ErrConversionFailed = apperror.Define(apperror.Internal, "image.conversion_failed", "image conversion failed")

	ErrRegistryUnauthenticated  = apperror.Define(apperror.Unauthenticated, "registry.unauthenticated", "registry authentication failed")
	ErrRegistryPermissionDenied = apperror.Define(apperror.PermissionDenied, "registry.permission_denied", "registry permission denied")
	ErrRegistryNotFound         = apperror.Define(apperror.NotFound, "registry.not_found", "registry content not found")
	ErrRegistryConflict         = apperror.Define(apperror.Conflict, "registry.conflict", "registry request conflicted")
	ErrRegistryRateLimited      = apperror.Define(apperror.ResourceExhausted, "registry.rate_limited", "registry rate limit exceeded")
	ErrRegistryUnavailable      = apperror.Define(apperror.Unavailable, "registry.unavailable", "registry is unavailable")
	ErrRegistryUpstreamFailure  = apperror.Define(apperror.UpstreamFailure, "registry.upstream_failure", "registry request failed")
)

func translateRegistryError(err error) error {
	if err == nil {
		return nil
	}
	var appErr *apperror.Error
	if errors.As(err, &appErr) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, containerdreference.ErrInvalid) ||
		errors.Is(err, containerdreference.ErrObjectRequired) ||
		errors.Is(err, containerdreference.ErrHostnameRequired) {
		return ErrInvalidArgument.Wrap(err)
	}
	if errdefs.IsNotFound(err) {
		return ErrRegistryNotFound.Wrap(err)
	}
	var unexpected remoteerrors.ErrUnexpectedStatus
	if errors.As(err, &unexpected) {
		switch unexpected.StatusCode {
		case http.StatusUnauthorized:
			return ErrRegistryUnauthenticated.Wrap(err)
		case http.StatusForbidden:
			return ErrRegistryPermissionDenied.Wrap(err)
		case http.StatusNotFound:
			return ErrRegistryNotFound.Wrap(err)
		case http.StatusConflict:
			return ErrRegistryConflict.Wrap(err)
		case http.StatusTooManyRequests:
			return ErrRegistryRateLimited.Wrap(err)
		case http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return ErrRegistryUnavailable.Wrap(err)
		default:
			return ErrRegistryUpstreamFailure.Wrap(err)
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrRegistryUnavailable.Wrap(err)
	}
	return err
}
