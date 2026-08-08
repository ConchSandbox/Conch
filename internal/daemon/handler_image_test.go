package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"
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

func TestWriteImageErrorMapsInvalidAuthorizationToUnauthorized(t *testing.T) {
	err := fmt.Errorf("pull access denied: %w", fmt.Errorf("%w: no basic auth credentials", docker.ErrInvalidAuthorization))
	rec := httptest.NewRecorder()
	writeImageError(rec, "Failed to pull image", err)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	const wantBody = "Failed to pull image: registry request failed: Unauthorized\n"
	if rec.Body.String() != wantBody {
		t.Fatalf("body = %q, want %q", rec.Body.String(), wantBody)
	}
	if strings.Contains(rec.Body.String(), "no basic auth credentials") || strings.Contains(rec.Body.String(), "pull access denied") {
		t.Fatalf("response body leaked registry authorization details: %q", rec.Body.String())
	}
}

func TestWriteImageErrorMapsResolverBasicAuthChallengeToUnauthorized(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}))
	defer registry.Close()

	registryHost := strings.TrimPrefix(registry.URL, "http://")
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: true,
		Credentials: func(string) (string, string, error) {
			return "", "", nil
		},
	})
	_, _, err := resolver.Resolve(context.Background(), registryHost+"/conch/test:latest")
	if !errors.Is(err, docker.ErrInvalidAuthorization) {
		t.Fatalf("resolver error = %v, want ErrInvalidAuthorization", err)
	}

	rec := httptest.NewRecorder()
	writeImageError(rec, "Failed to pull image", fmt.Errorf("pull image: %w", err))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	const wantBody = "Failed to pull image: registry request failed: Unauthorized\n"
	if rec.Body.String() != wantBody {
		t.Fatalf("body = %q, want %q", rec.Body.String(), wantBody)
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
