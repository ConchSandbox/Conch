package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

func TestHandlePullImageUsesDefaultKernel(t *testing.T) {
	svc := &fakeImageService{results: map[string]string{"rootfs": "rootfs-id"}}
	server := newImageHandlerServer(svc)

	body := bytes.NewBufferString(`{"image_name":"docker.io/library/nginx:latest","namespace":"team-a"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull", body)
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.pullReq.DefaultKernelImage != "hub.oepkgs.net/conch/kernel:6.6.0" {
		t.Fatalf("default kernel = %q", svc.pullReq.DefaultKernelImage)
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
			Kind:         "sandbox-base",
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
	if listResp.Images[0].Kind != "sandbox-base" {
		t.Fatalf("list response kind = %q, want sandbox-base", listResp.Images[0].Kind)
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

func TestHandleConvertImage(t *testing.T) {
	imgSvc := &fakeImageService{}
	snapSvc := &fakeSnapshotService{}
	server := newConvertHandlerServer(imgSvc, snapSvc, nil)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("metadata", `{"source":" docker.io/library/nginx:latest ","namespace":"team-a","boot_index_tag":" localhost/conch/demo:latest ","plain_http":true}`)
	kernelPart, err := writer.CreateFormFile("kernel", "vmlinuz")
	if err != nil {
		t.Fatalf("kernel form file: %v", err)
	}
	_, _ = kernelPart.Write([]byte("kernel"))
	initrdPart, err := writer.CreateFormFile("initrd", "conch.initrd")
	if err != nil {
		t.Fatalf("initrd form file: %v", err)
	}
	_, _ = initrdPart.Write([]byte("initrd"))
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/convert", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if imgSvc.prepareReq.Source != "docker.io/library/nginx:latest" || imgSvc.prepareReq.Namespace != "team-a" || !imgSvc.prepareReq.PlainHTTP {
		t.Fatalf("prepare request = %#v", imgSvc.prepareReq)
	}
	if imgSvc.convertReq.Namespace != "team-a" || imgSvc.convertReq.SourceImage != "docker.io/library/nginx:latest" {
		t.Fatalf("convert request = %#v", imgSvc.convertReq)
	}
	if imgSvc.publishReq.Namespace != "team-a" || imgSvc.publishReq.RootfsImageName == "" || imgSvc.publishReq.BootIndexTag != "localhost/conch/demo:latest" {
		t.Fatalf("publish request = %#v", imgSvc.publishReq)
	}
	if imgSvc.publishReq.KernelPath == "" || imgSvc.publishReq.InitrdPath == "" {
		t.Fatalf("publish request missing kernel/initrd paths: %#v", imgSvc.publishReq)
	}
	if len(imgSvc.importReqs) != 0 {
		t.Fatalf("import requests = %#v, want none", imgSvc.importReqs)
	}
	var got convertImageResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.BootIndexTag != "localhost/conch/demo:latest" || got.BootIndexDigest == "" || got.RootfsImageRef == "" {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleConvertImageSnapshotPauseFailureCleansSandbox(t *testing.T) {
	imgSvc := &fakeImageService{}
	snapSvc := &fakeSnapshotService{}
	manager := &fakeSandboxOps{pauseErr: errors.New("pause failed")}
	server := newConvertHandlerServer(imgSvc, snapSvc, manager)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("metadata", `{"source":"docker.io/library/nginx:latest","namespace":"team-a","boot_index_tag":"localhost/conch/demo:latest","snapshot":true}`)
	kernelPart, err := writer.CreateFormFile("kernel", "vmlinuz")
	if err != nil {
		t.Fatalf("kernel form file: %v", err)
	}
	_, _ = kernelPart.Write([]byte("kernel"))
	initrdPart, err := writer.CreateFormFile("initrd", "conch.initrd")
	if err != nil {
		t.Fatalf("initrd form file: %v", err)
	}
	_, _ = initrdPart.Write([]byte("initrd"))
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/convert", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if manager.createReq.SandboxId == "" {
		t.Fatalf("sandbox was not created: %#v", manager.createReq)
	}
	if imgSvc.publishReq.BootIndexTag == "localhost/conch/demo:latest" || !strings.HasPrefix(imgSvc.publishReq.BootIndexTag, "conch-boot:convert-") {
		t.Fatalf("publish request used final tag instead of temporary tag: %#v", imgSvc.publishReq)
	}
	if manager.createReq.ImageName != imgSvc.publishReq.BootIndexTag {
		t.Fatalf("sandbox image = %q, want temporary boot image %q", manager.createReq.ImageName, imgSvc.publishReq.BootIndexTag)
	}
	if len(manager.deleteReqs) != 1 {
		t.Fatalf("delete requests = %#v, want one cleanup", manager.deleteReqs)
	}
	if manager.deleteReqs[0].SandboxId != manager.createReq.SandboxId || manager.deleteReqs[0].Namespace != "team-a" {
		t.Fatalf("cleanup request = %#v, create request = %#v", manager.deleteReqs[0], manager.createReq)
	}
	if imgSvc.removeReq.ImageName != imgSvc.publishReq.BootIndexTag || imgSvc.removeReq.Namespace != "team-a" {
		t.Fatalf("temporary image cleanup request = %#v, publish request = %#v", imgSvc.removeReq, imgSvc.publishReq)
	}
}

func fmtInvalidImageName() error {
	return errors.Join(conchimage.ErrInvalidRequest, errors.New("image_name is required"))
}
