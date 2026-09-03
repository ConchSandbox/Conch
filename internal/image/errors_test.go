package image

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	remoteerrors "github.com/containerd/containerd/v2/core/remotes/errors"
	containerdreference "github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/errdefs"
	"github.com/openeuler/Conch/internal/apperror"
)

func TestTranslateRegistryStatus(t *testing.T) {
	tests := []struct {
		status int
		want   *apperror.Error
	}{
		{http.StatusUnauthorized, ErrRegistryUnauthenticated},
		{http.StatusForbidden, ErrRegistryPermissionDenied},
		{http.StatusNotFound, ErrRegistryNotFound},
		{http.StatusConflict, ErrRegistryConflict},
		{http.StatusTooManyRequests, ErrRegistryRateLimited},
		{http.StatusServiceUnavailable, ErrRegistryUnavailable},
		{http.StatusInternalServerError, ErrRegistryUpstreamFailure},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			cause := remoteerrors.ErrUnexpectedStatus{
				StatusCode: tt.status,
				RequestURL: "https://user:secret@registry.example.invalid/private",
			}
			err := translateRegistryError(fmt.Errorf("registry operation: %w", cause))
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %s", err, tt.want.Code())
			}
			var retained remoteerrors.ErrUnexpectedStatus
			if !errors.As(err, &retained) {
				t.Fatal("registry cause was not retained")
			}
		})
	}
}

func TestTranslateRegistryErrorPreservesExistingClassificationAndDeadline(t *testing.T) {
	classified := ErrInvalidArgument.Wrap(errors.New("bad reference"))
	if got := translateRegistryError(classified); got != classified {
		t.Fatalf("existing classification was replaced: %v", got)
	}
	if got := translateRegistryError(context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("deadline was replaced: %v", got)
	}
}

func TestTranslateRegistryErrorClassifiesResolverNotFound(t *testing.T) {
	cause := fmt.Errorf("resolve manifest: %w", errdefs.ErrNotFound)
	got := translateRegistryError(cause)
	if !errors.Is(got, ErrRegistryNotFound) {
		t.Fatalf("error = %v, want %s", got, ErrRegistryNotFound.Code())
	}
	if !errors.Is(got, errdefs.ErrNotFound) {
		t.Fatal("resolver cause was not retained")
	}
}

func TestTranslateRegistryErrorClassifiesInvalidReference(t *testing.T) {
	resolver := docker.NewResolver(docker.ResolverOptions{})
	for _, tc := range []struct {
		reference string
		cause     error
	}{
		{reference: "12345", cause: containerdreference.ErrObjectRequired},
		{reference: "registry.example/team/image", cause: containerdreference.ErrObjectRequired},
		{reference: "https://registry.example/team/image:latest", cause: containerdreference.ErrInvalid},
		{reference: "/team/image:latest", cause: containerdreference.ErrHostnameRequired},
	} {
		_, _, cause := resolver.Resolve(context.Background(), tc.reference)
		if !errors.Is(cause, tc.cause) {
			t.Fatalf("Resolve(%q) error = %v, want %v", tc.reference, cause, tc.cause)
		}
		got := translateRegistryError(cause)
		if !errors.Is(got, ErrInvalidArgument) {
			t.Fatalf("error = %v, want %s", got, ErrInvalidArgument.Code())
		}
		if !errors.Is(got, tc.cause) {
			t.Fatalf("error = %v, want cause %v", got, tc.cause)
		}
	}
}

func TestTranslateRegistryErrorPreservesUnknownFailure(t *testing.T) {
	cause := errors.New("content store I/O failed")
	err := fmt.Errorf("classify fetched image: %w", cause)
	got := translateRegistryError(err)
	if got != err {
		t.Fatalf("unknown failure was replaced: %v", got)
	}
	var appErr *apperror.Error
	if errors.As(got, &appErr) {
		t.Fatalf("unknown failure was classified as %s", appErr.Code())
	}
}
