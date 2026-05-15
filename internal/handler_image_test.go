package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	imageSvc "github.com/openeuler/Conch/internal/conchservices/image"
	snapshotSvc "github.com/openeuler/Conch/internal/conchservices/snapshot"
)

type fakeImageService struct {
	pullReq   imageSvc.PullRequest
	unpackReq imageSvc.UnpackRequest
	pullErr   error
	unpackErr error
	results   map[string]string
	importReq imageSvc.ImportArchiveRequest
	importRaw string
}

type fakeSnapshotService struct {
	linkReq  snapshotSvc.LinkVMRequest
	infoReq  snapshotSvc.InfoRequest
	chainReq snapshotSvc.InfoRequest
	linkErr  error
	infoErr  error
	chainErr error
}

func (f *fakeImageService) Pull(_ context.Context, req imageSvc.PullRequest) (map[string]string, error) {
	f.pullReq = req
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return f.results, nil
}

func (f *fakeImageService) Unpack(_ context.Context, req imageSvc.UnpackRequest) (map[string]string, error) {
	f.unpackReq = req
	if f.unpackErr != nil {
		return nil, f.unpackErr
	}
	return f.results, nil
}

func (f *fakeImageService) ImportArchive(_ context.Context, archive io.Reader, req imageSvc.ImportArchiveRequest) (imageSvc.ImportArchiveResponse, error) {
	f.importReq = req
	raw, _ := io.ReadAll(archive)
	f.importRaw = string(raw)
	return imageSvc.ImportArchiveResponse{SnapshotKey: "rootfs-id", ImageName: "image:latest"}, nil
}

func newImageHandlerServer(svc imageService) *Server {
	s := &Server{
		router:        http.NewServeMux(),
		imageService:  svc,
		defaultKernel: "hub.oepkgs.net/conch/kernel:6.6.0",
	}
	s.routes()
	return s
}

func newSnapshotHandlerServer(svc snapshotService) *Server {
	s := &Server{
		router:          http.NewServeMux(),
		snapshotService: svc,
	}
	s.routes()
	return s
}

func (f *fakeSnapshotService) LinkVM(_ context.Context, req snapshotSvc.LinkVMRequest) error {
	f.linkReq = req
	return f.linkErr
}

func (f *fakeSnapshotService) Info(_ context.Context, req snapshotSvc.InfoRequest) (snapshotSvc.Meta, error) {
	f.infoReq = req
	if f.infoErr != nil {
		return snapshotSvc.Meta{}, f.infoErr
	}
	return snapshotSvc.Meta{
		Key:         req.Key,
		Parent:      "parent-id",
		StoragePath: "/snap/rootfs",
	}, nil
}

func (f *fakeSnapshotService) Chain(_ context.Context, req snapshotSvc.InfoRequest) (snapshotSvc.Chain, error) {
	f.chainReq = req
	if f.chainErr != nil {
		return snapshotSvc.Chain{}, f.chainErr
	}
	return snapshotSvc.Chain{
		Info: snapshotSvc.Meta{
			Key:         req.Key,
			StoragePath: "/snap/rootfs",
		},
		ChainPaths: []string{"/snap/parent", "/snap/rootfs"},
	}, nil
}

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
	svc := &fakeImageService{pullErr: errors.Join(imageSvc.ErrOCIConversionFailed, errors.New("convert rootfs"))}
	server := newImageHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull", bytes.NewBufferString(`{"image_name":"docker.io/library/nginx:latest"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleImportImage(t *testing.T) {
	svc := &fakeImageService{}
	server := newImageHandlerServer(svc)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("namespace", "team-a")
	_ = writer.WriteField("imported_tag", "buildah-oci-rootfs:latest")
	part, err := writer.CreateFormFile("archive", "image.tar")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte("archive-content")); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.importReq.Namespace != "team-a" || svc.importReq.ImportedTag != "buildah-oci-rootfs:latest" {
		t.Fatalf("import request = %#v", svc.importReq)
	}
	if svc.importRaw != "archive-content" {
		t.Fatalf("archive = %q", svc.importRaw)
	}
	var got imageSvc.ImportArchiveResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.SnapshotKey != "rootfs-id" || got.ImageName != "image:latest" {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleLinkSnapshotVM(t *testing.T) {
	svc := &fakeSnapshotService{}
	server := newSnapshotHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/link-vm", bytes.NewBufferString(`{"rootfs_snapshot_id":"rootfs-id","vm_snapshot_id":"vm-id","namespace":"team-a"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.linkReq.RootfsSnapshotID != "rootfs-id" || svc.linkReq.VMSnapshotID != "vm-id" || svc.linkReq.Namespace != "team-a" {
		t.Fatalf("link request = %#v", svc.linkReq)
	}
}

func TestHandleLinkSnapshotVMValidatesRequest(t *testing.T) {
	server := newSnapshotHandlerServer(&fakeSnapshotService{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/link-vm", bytes.NewBufferString(`{"rootfs_snapshot_id":"rootfs-id"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSnapshotInfo(t *testing.T) {
	svc := &fakeSnapshotService{}
	server := newSnapshotHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/info", bytes.NewBufferString(`{"key":"rootfs-id","namespace":"team-a"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.infoReq.Key != "rootfs-id" || svc.infoReq.Namespace != "team-a" {
		t.Fatalf("info request = %#v", svc.infoReq)
	}
	var got snapshotSvc.Meta
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Key != "rootfs-id" || got.StoragePath != "/snap/rootfs" {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleSnapshotChain(t *testing.T) {
	svc := &fakeSnapshotService{}
	server := newSnapshotHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/chain", bytes.NewBufferString(`{"key":"rootfs-id","namespace":"team-a"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.chainReq.Key != "rootfs-id" || svc.chainReq.Namespace != "team-a" {
		t.Fatalf("chain request = %#v", svc.chainReq)
	}
	var got snapshotSvc.Chain
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.ChainPaths) != 2 || got.ChainPaths[1] != "/snap/rootfs" {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleSnapshotServiceUnavailable(t *testing.T) {
	server := newSnapshotHandlerServer(nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/info", bytes.NewBufferString(`{"key":"rootfs-id"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func fmtInvalidImageName() error {
	return errors.Join(imageSvc.ErrInvalidRequest, errors.New("image_name is required"))
}
