package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

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

func TestHandlePullImageStreamWritesProgressEvents(t *testing.T) {
	svc := &fakeImageService{
		results: map[string]string{"rootfs": "rootfs-id"},
		progress: []runtimeapi.PullProgress{{
			Status:    "downloading",
			Component: "rootfs",
			Progress:  40,
			Total:     100,
		}, {
			Status: "unpacking",
		}},
	}
	server := newImageHandlerServer(svc)

	body := bytes.NewBufferString(`{"image_name":"hub.oepkgs.net/conch/demo:latest","namespace":"team-a"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull/stream", body)
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/x-ndjson") {
		t.Fatalf("content type = %q, want application/x-ndjson", got)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("stream response has %d lines, want at least 2: %q", len(lines), rec.Body.String())
	}
	var first, progress, unpacking, last struct {
		Status    string            `json:"status"`
		Component string            `json:"component"`
		Progress  int64             `json:"progress"`
		Total     int64             `json:"total"`
		Results   map[string]string `json:"results"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode first event: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &progress); err != nil {
		t.Fatalf("decode progress event: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &unpacking); err != nil {
		t.Fatalf("decode unpacking event: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("decode last event: %v", err)
	}
	if first.Status != "started" || progress.Status != "downloading" || progress.Component != "rootfs" || progress.Progress != 40 || progress.Total != 100 || unpacking.Status != "unpacking" || last.Status != "completed" || last.Results["rootfs"] != "rootfs-id" {
		t.Fatalf("events first=%#v progress=%#v unpacking=%#v last=%#v", first, progress, unpacking, last)
	}
}

func TestHandlePullImageStreamForwardsSkipUnpack(t *testing.T) {
	svc := &fakeImageService{}
	server := newImageHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull/stream", bytes.NewBufferString(`{"image_name":"hub.oepkgs.net/conch/demo:latest","skip_unpack":true}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !svc.pullReq.SkipUnpack {
		t.Fatalf("stream pull request did not forward skip_unpack: %#v", svc.pullReq)
	}
	if strings.Contains(rec.Body.String(), `"status":"unpacking"`) {
		t.Fatalf("skip-unpack stream should not report unpacking:\n%s", rec.Body.String())
	}
}

func TestHandlePullImageStreamDoesNotDropProgressEvents(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	progress := make([]runtimeapi.PullProgress, 0, 40)
	for i := 0; i < 40; i++ {
		progress = append(progress, runtimeapi.PullProgress{
			Status:    "downloading",
			Component: "rootfs",
			Progress:  int64(i),
			Total:     100,
		})
	}
	progress = append(progress, runtimeapi.PullProgress{Status: "unpacking"})
	svc := &fakeImageService{
		results:  map[string]string{"rootfs": "rootfs-id"},
		progress: progress,
	}
	server := newImageHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull/stream", bytes.NewBufferString(`{"image_name":"hub.oepkgs.net/conch/demo:latest"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"progress":39`) {
		t.Fatalf("stream response dropped the last byte progress event:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"unpacking"`) {
		t.Fatalf("stream response dropped unpacking event:\n%s", rec.Body.String())
	}
}

func TestHandlePullImageStreamCancelsRuntimeOnClientCancel(t *testing.T) {
	svc := &fakeImageService{
		pullStarted:  make(chan struct{}),
		pullCanceled: make(chan struct{}),
	}
	server := newImageHandlerServer(svc)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull/stream", bytes.NewBufferString(`{"image_name":"hub.oepkgs.net/conch/demo:latest"}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		server.router.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	select {
	case <-svc.pullStarted:
	case <-time.After(time.Second):
		t.Fatal("runtime pull did not start")
	}
	cancel()

	select {
	case <-svc.pullCanceled:
	case <-time.After(time.Second):
		t.Fatal("runtime pull context was not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream handler did not return after client cancellation")
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
