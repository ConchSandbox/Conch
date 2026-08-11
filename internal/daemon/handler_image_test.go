package daemon

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/apperror"
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

func TestWriteAPIErrorClassifiesImageErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "invalid request", err: conchimage.ErrInvalidArgument.Wrap(errors.New("image_name is required")), wantStatus: http.StatusBadRequest},
		{name: "conversion failure", err: conchimage.ErrConversionFailed.Wrap(errors.New("convert rootfs")), wantStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeAPIError(rec, test.err)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, test.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestWriteImageErrorPreservesRegistryAuthStatusWithoutLeakingCredentials(t *testing.T) {
	const (
		registryUsername = "registry-user"
		registryPassword = "registry-password"
	)
	for _, test := range []struct {
		name       string
		statusCode int
		prototype  *apperror.Error
	}{
		{name: "Unauthorized", statusCode: http.StatusUnauthorized, prototype: conchimage.ErrRegistryUnauthenticated},
		{name: "Forbidden", statusCode: http.StatusForbidden, prototype: conchimage.ErrRegistryPermissionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeAPIError(rec, test.prototype.Wrap(errors.New("https://"+registryUsername+":"+registryPassword+"@registry.example.invalid/private response")))

			if rec.Code != test.statusCode {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, test.statusCode, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), registryUsername) || strings.Contains(rec.Body.String(), registryPassword) {
				t.Fatalf("response body leaked registry credentials: %q", rec.Body.String())
			}
		})
	}
}

func TestWriteAPIErrorOnlyTrustsApplicationClassification(t *testing.T) {
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
			name:       "classified registry status",
			wantStatus: http.StatusTooManyRequests,
			err:        conchimage.ErrRegistryRateLimited.New(),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeAPIError(rec, test.err)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, test.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestRemovedImageRoutesReturnNotFound(t *testing.T) {
	server := &Daemon{router: http.NewServeMux()}
	server.routes()

	for _, path := range []string{"/api/image/convert", "/api/image/import", "/api/image/unpack"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}"))
			server.router.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}
