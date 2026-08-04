package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	remoteerrors "github.com/containerd/containerd/v2/core/remotes/errors"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

func TestHandlePullImageForwardsTargetImageOptions(t *testing.T) {
	svc := &fakeImageService{results: map[string]string{"rootfs": "rootfs-id"}}
	server := newImageHandlerServer(svc)

	body := bytes.NewBufferString(`{"image_name":"docker.io/library/nginx:latest","namespace":"team-a","plain_http":true,"username":"user","password":"pass","skip_unpack":true}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull", body)
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.pullReq.ImageName != "docker.io/library/nginx:latest" ||
		svc.pullReq.Namespace != "team-a" ||
		!svc.pullReq.PlainHTTP ||
		svc.pullReq.Username != "user" ||
		svc.pullReq.Password != "pass" ||
		!svc.pullReq.SkipUnpack {
		t.Fatalf("pull request = %#v", svc.pullReq)
	}
	var got struct {
		Results map[string]string `json:"results"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Results["rootfs"] != "rootfs-id" {
		t.Fatalf("results[rootfs] = %q", got.Results["rootfs"])
	}
}

func TestHandleUnpackImage(t *testing.T) {
	svc := &fakeImageService{results: map[string]string{"sandbox": "vm-id"}}
	server := newImageHandlerServer(svc)

	body := bytes.NewBufferString(`{"image_name":"hub.oepkgs.net/conch/conch-index:v0.1","namespace":"default"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/unpack", body)
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.unpackReq.ImageName != "hub.oepkgs.net/conch/conch-index:v0.1" {
		t.Fatalf("unpack image = %q", svc.unpackReq.ImageName)
	}
}

func TestHandleListAndRemoveImage(t *testing.T) {
	svc := &fakeImageService{
		images: []runtimeapi.ImageRecord{{
			Name:         "localhost/conch/demo:latest",
			TargetDigest: "sha256:demo",
			Size:         42,
			Kind:         "boot-index-cold",
		}},
	}
	server := newImageHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/list", bytes.NewBufferString(`{"namespace":"team-a","filters":["name==localhost/conch/demo:latest"]}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.listReq.Namespace != "team-a" || len(svc.listReq.Filters) != 1 {
		t.Fatalf("list request = %#v", svc.listReq)
	}
	var listResp listImageResponse
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Images) != 1 || listResp.Images[0].Name != "localhost/conch/demo:latest" {
		t.Fatalf("list response = %#v", listResp)
	}
	if listResp.Images[0].TargetDigest != "sha256:demo" {
		t.Fatalf("list response target digest = %q, want sha256:demo", listResp.Images[0].TargetDigest)
	}
	if listResp.Images[0].Kind != "boot-index-cold" {
		t.Fatalf("list response kind = %q, want boot-index-cold", listResp.Images[0].Kind)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/image/remove", bytes.NewBufferString(`{"namespace":"team-a","image_name":"localhost/conch/demo:latest","synchronous":true}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.removeReq.Namespace != "team-a" || svc.removeReq.ImageName != "localhost/conch/demo:latest" || !svc.removeReq.Synchronous {
		t.Fatalf("remove request = %#v", svc.removeReq)
	}
}

func TestHandlePullImageUnavailable(t *testing.T) {
	server := newImageHandlerServer(nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull", bytes.NewBufferString(`{"image_name":"x"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleImageServiceBadRequest(t *testing.T) {
	svc := &fakeImageService{pullErr: fmtInvalidImageName()}
	server := newImageHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull", bytes.NewBufferString(`{"image_name":""}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePullImageConversionFailureIsBadRequest(t *testing.T) {
	svc := &fakeImageService{pullErr: errors.Join(conchimage.ErrOCIConversionFailed, errors.New("convert rootfs"))}
	server := newImageHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull", bytes.NewBufferString(`{"image_name":"docker.io/library/nginx:latest"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePullImagePreservesRegistryAuthStatus(t *testing.T) {
	const (
		registryUsername = "registry-user"
		registryPassword = "registry-password"
	)
	tests := []struct {
		name       string
		statusCode int
		wrap       func(error) error
	}{
		{
			name:       "unauthorized",
			statusCode: http.StatusUnauthorized,
			wrap: func(err error) error {
				return fmt.Errorf("pull failed: resolve failed: %w", err)
			},
		},
		{
			name:       "forbidden",
			statusCode: http.StatusForbidden,
			wrap: func(err error) error {
				return errors.Join(errors.New("pull failed"), fmt.Errorf("resolve failed: %w", err))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registryErr := remoteerrors.ErrUnexpectedStatus{
				Status:        fmt.Sprintf("%d %s", tt.statusCode, http.StatusText(tt.statusCode)),
				StatusCode:    tt.statusCode,
				RequestMethod: http.MethodGet,
				RequestURL:    "https://" + registryUsername + ":" + registryPassword + "@registry.example.invalid/v2/conch/manifests/latest",
			}
			server := newImageHandlerServer(&fakeImageService{pullErr: tt.wrap(registryErr)})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/image/pull", bytes.NewBufferString(`{"image_name":"registry.example.invalid/conch:latest"}`))
			server.router.ServeHTTP(rec, req)

			if rec.Code != tt.statusCode {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.statusCode, rec.Body.String())
			}
			wantBody := "Failed to pull image: registry request failed: " + http.StatusText(tt.statusCode) + "\n"
			if rec.Body.String() != wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), wantBody)
			}
			if strings.Contains(rec.Body.String(), registryUsername) || strings.Contains(rec.Body.String(), registryPassword) {
				t.Fatalf("response body leaked registry credentials: %q", rec.Body.String())
			}
		})
	}
}

func TestHandlePullImagePreservesRegistryStatus(t *testing.T) {
	tests := []struct {
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newImageHandlerServer(&fakeImageService{pullErr: tt.err})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/image/pull", bytes.NewBufferString(`{"image_name":"registry.example.invalid/conch:latest"}`))
			server.router.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestConvertImageRouteRemoved(t *testing.T) {
	imgSvc := &fakeImageService{}
	snapSvc := &fakeSnapshotService{}
	server := newConvertHandlerServer(imgSvc, imgSvc, snapSvc, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/convert", bytes.NewBufferString("{}"))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func fmtInvalidImageName() error {
	return errors.Join(conchimage.ErrInvalidRequest, errors.New("image_name is required"))
}
