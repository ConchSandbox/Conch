package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	remoteerrors "github.com/containerd/containerd/v2/core/remotes/errors"
	"github.com/openeuler/Conch/internal/conchruntime"
	conchimage "github.com/openeuler/Conch/internal/image"
)

func TestHandlePullImageUnavailable(t *testing.T) {
	runtimeService := conchruntime.New(nil, nil, nil)
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull", bytes.NewBufferString(`{"image_name":"x"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestWriteImageErrorClassifiesClientErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "invalid request", err: errors.Join(conchimage.ErrInvalidRequest, errors.New("image_name is required"))},
		{name: "conversion failure", err: errors.Join(conchimage.ErrOCIConversionFailed, errors.New("convert rootfs"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeImageError(rec, "Failed to pull image", test.err)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func TestWriteImageErrorPreservesRegistryAuthStatusWithoutLeakingCredentials(t *testing.T) {
	const (
		registryUsername = "registry-user"
		registryPassword = "registry-password"
	)
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			registryErr := remoteerrors.ErrUnexpectedStatus{
				Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
				StatusCode:    statusCode,
				RequestMethod: http.MethodGet,
				RequestURL:    "https://" + registryUsername + ":" + registryPassword + "@registry.example.invalid/v2/conch/manifests/latest",
			}
			rec := httptest.NewRecorder()
			writeImageError(rec, "Failed to pull image", fmt.Errorf("pull failed: %w", registryErr))

			if rec.Code != statusCode {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, statusCode, rec.Body.String())
			}
			wantBody := "Failed to pull image: registry request failed: " + http.StatusText(statusCode) + "\n"
			if rec.Body.String() != wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), wantBody)
			}
			if strings.Contains(rec.Body.String(), registryUsername) || strings.Contains(rec.Body.String(), registryPassword) {
				t.Fatalf("response body leaked registry credentials: %q", rec.Body.String())
			}
		})
	}
}

func TestWriteImageErrorOnlyTrustsTypedRegistryStatus(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "plain auth-like message",
			err:        errors.New("registry returned 401 Unauthorized"),
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "typed registry status",
			wantStatus: http.StatusTooManyRequests,
			err: remoteerrors.ErrUnexpectedStatus{
				Status:        "429 Too Many Requests",
				StatusCode:    http.StatusTooManyRequests,
				RequestMethod: http.MethodGet,
				RequestURL:    "https://registry.example.invalid/v2/conch/manifests/latest",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeImageError(rec, "Failed to pull image", test.err)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, test.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestConvertImageRouteRemoved(t *testing.T) {
	server := &Daemon{router: http.NewServeMux()}
	server.routes()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/convert", bytes.NewBufferString("{}"))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}
