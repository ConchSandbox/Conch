package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/opencontainers/go-digest"
	imageSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/image"
	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	"github.com/openeuler/Conch/internal/conchruntime"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/snapshot/common"
)

type fakeImageService struct {
	pullReq     imageSvc.PullRequest
	pushReq     imageSvc.PushRequest
	listReq     imageSvc.ListRequest
	removeReq   imageSvc.RemoveRequest
	unpackReq   imageSvc.UnpackRequest
	prepareReq  imageSvc.PrepareRootfsSourceRequest
	convertReq  imageSvc.ConvertRootfsToErofsRequest
	exportReq   imageSvc.ExportArchiveRequest
	pullErr     error
	pushErr     error
	listErr     error
	removeErr   error
	unpackErr   error
	prepareErr  error
	convertErr  error
	exportErr   error
	results     map[string]string
	importReqs  []imageSvc.ImportArchiveRequest
	importRaws  []string
	exportRaw   string
	prepareResp imageSvc.PrepareRootfsSourceResponse
	convertResp imageSvc.ConvertRootfsToErofsResponse
	images      []imageSvc.Meta
}

type fakeSnapshotService struct {
	linkReq   snapshotSvc.LinkVMRequest
	listReq   snapshotSvc.ListRequest
	removeReq snapshotSvc.RemoveRequest
	infoReq   snapshotSvc.InfoRequest
	chainReq  snapshotSvc.InfoRequest
	linkErr   error
	listErr   error
	removeErr error
	infoErr   error
	chainErr  error
	snapshots []snapshotSvc.Meta
}

type fakeSandboxOps struct {
	createReq sandbox.SandboxCreateRequest
	pauseReq  sandbox.SandboxPauseRequest
	createErr error
	pauseErr  error
}

func (f *fakeImageService) Pull(_ context.Context, req imageSvc.PullRequest) (map[string]string, error) {
	f.pullReq = req
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return f.results, nil
}

func (f *fakeImageService) Push(_ context.Context, req imageSvc.PushRequest) error {
	f.pushReq = req
	return f.pushErr
}

func (f *fakeImageService) List(_ context.Context, req imageSvc.ListRequest) ([]imageSvc.Meta, error) {
	f.listReq = req
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.images, nil
}

func (f *fakeImageService) Remove(_ context.Context, req imageSvc.RemoveRequest) error {
	f.removeReq = req
	return f.removeErr
}

func (f *fakeImageService) Unpack(_ context.Context, req imageSvc.UnpackRequest) (map[string]string, error) {
	f.unpackReq = req
	if f.unpackErr != nil {
		return nil, f.unpackErr
	}
	return f.results, nil
}

func (f *fakeImageService) ImportArchive(_ context.Context, archive io.Reader, req imageSvc.ImportArchiveRequest) (imageSvc.ImportArchiveResponse, error) {
	f.importReqs = append(f.importReqs, req)
	raw, _ := io.ReadAll(archive)
	f.importRaws = append(f.importRaws, string(raw))
	return imageSvc.ImportArchiveResponse{SnapshotKey: req.ImportedTag + "-snapshot", ImageName: req.ImportedTag}, nil
}

func (f *fakeImageService) ExportArchive(_ context.Context, w io.Writer, req imageSvc.ExportArchiveRequest) error {
	f.exportReq = req
	if f.exportErr != nil {
		return f.exportErr
	}
	_, _ = io.WriteString(w, "archive-content")
	f.exportRaw = "archive-content"
	return nil
}

func (f *fakeImageService) PrepareRootfsSource(_ context.Context, req imageSvc.PrepareRootfsSourceRequest) (imageSvc.PrepareRootfsSourceResponse, error) {
	f.prepareReq = req
	if f.prepareErr != nil {
		return imageSvc.PrepareRootfsSourceResponse{}, f.prepareErr
	}
	if f.prepareResp.ImageName != "" {
		return f.prepareResp, nil
	}
	imageName := req.Source
	if req.TargetImage != "" {
		imageName = req.TargetImage
	}
	return imageSvc.PrepareRootfsSourceResponse{
		ImageName:      imageName,
		ManifestDigest: "sha256:manifest",
	}, nil
}

func (f *fakeImageService) ConvertRootfsToErofs(_ context.Context, req imageSvc.ConvertRootfsToErofsRequest) (imageSvc.ConvertRootfsToErofsResponse, error) {
	f.convertReq = req
	if f.convertErr != nil {
		return imageSvc.ConvertRootfsToErofsResponse{}, f.convertErr
	}
	if f.convertResp.ImageName != "" {
		return f.convertResp, nil
	}
	return imageSvc.ConvertRootfsToErofsResponse{
		ImageName:      req.TargetImage,
		ManifestDigest: "sha256:manifest",
		SnapshotKey:    "rootfs-id",
	}, nil
}

func newRuntimeForTest(image conchruntime.ImageOps, snapshot conchruntime.SnapshotOps, sandboxOps conchruntime.SandboxOps) *conchruntime.Service {
	rt := conchruntime.New(sandboxOps, image, nil, "default")
	rt.Snapshot = snapshot
	return rt
}

func newImageHandlerServer(svc conchruntime.ImageOps) *Daemon {
	s := &Daemon{
		router:         http.NewServeMux(),
		runtimeService: newRuntimeForTest(svc, nil, nil),
		defaultKernel:  "hub.oepkgs.net/conch/kernel:6.6.0",
	}
	s.routes()
	return s
}

func newSnapshotHandlerServer(svc conchruntime.SnapshotOps) *Daemon {
	s := &Daemon{
		router:         http.NewServeMux(),
		runtimeService: newRuntimeForTest(nil, svc, nil),
	}
	s.routes()
	return s
}

func newConvertHandlerServer(imageSvc conchruntime.ImageOps, snapshotSvc conchruntime.SnapshotOps, sandboxOps conchruntime.SandboxOps) *Daemon {
	s := &Daemon{
		router:         http.NewServeMux(),
		runtimeService: newRuntimeForTest(imageSvc, snapshotSvc, sandboxOps),
	}
	s.routes()
	return s
}

func (f *fakeSnapshotService) LinkVM(_ context.Context, req snapshotSvc.LinkVMRequest) error {
	f.linkReq = req
	return f.linkErr
}

func (f *fakeSnapshotService) List(_ context.Context, req snapshotSvc.ListRequest) ([]snapshotSvc.Meta, error) {
	f.listReq = req
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.snapshots, nil
}

func (f *fakeSnapshotService) Remove(_ context.Context, req snapshotSvc.RemoveRequest) error {
	f.removeReq = req
	return f.removeErr
}

func (f *fakeSnapshotService) Info(_ context.Context, req snapshotSvc.InfoRequest) (snapshotSvc.Meta, error) {
	f.infoReq = req
	if f.infoErr != nil {
		return snapshotSvc.Meta{}, f.infoErr
	}
	return snapshotSvc.Meta{
		Key:    req.Key,
		Parent: "parent-id",
		Labels: map[string]string{
			common.SnapshotLabelMemSnapshot: "mem-id",
			common.SnapshotLabelVMSnapshot:  "vm-id",
			common.SnapshotLabelRootfsImage: "rootfs-image:latest",
		},
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

func (f *fakeSandboxOps) Create(req sandbox.SandboxCreateRequest) (sandbox.SandboxCreateResult, error) {
	f.createReq = req
	if f.createErr != nil {
		return sandbox.SandboxCreateResult{}, f.createErr
	}
	return sandbox.SandboxCreateResult{IP: "192.0.2.2", Namespace: req.Namespace, SandboxID: req.SandboxId}, nil
}

func (f *fakeSandboxOps) Delete(req sandbox.SandboxDeleteRequest) error {
	return nil
}

func (f *fakeSandboxOps) Pause(req sandbox.SandboxPauseRequest) (string, error) {
	f.pauseReq = req
	if f.pauseErr != nil {
		return "", f.pauseErr
	}
	return "paused-rootfs-id", nil
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

func TestHandleListAndRemoveImage(t *testing.T) {
	svc := &fakeImageService{
		images: []imageSvc.Meta{{
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
	var listResp struct {
		Images []imageSvc.Meta `json:"images"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Images) != 1 || listResp.Images[0].Name != "localhost/conch/demo:latest" {
		t.Fatalf("list response = %#v", listResp)
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
	svc := &fakeImageService{pullErr: errors.Join(imageSvc.ErrOCIConversionFailed, errors.New("convert rootfs"))}
	server := newImageHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/pull", bytes.NewBufferString(`{"image_name":"docker.io/library/nginx:latest"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleConvertImage(t *testing.T) {
	oldKernel := buildKernelArchiveFromFiles
	oldBoot := buildBootIndexArchive
	defer func() {
		buildKernelArchiveFromFiles = oldKernel
		buildBootIndexArchive = oldBoot
	}()
	buildKernelArchiveFromFiles = func(_ context.Context, _, _, _, archivePath string) (digest.Digest, error) {
		return digest.FromString("kernel"), os.WriteFile(archivePath, []byte("kernel-archive"), 0o644)
	}
	buildBootIndexArchive = func(_ context.Context, opts conchimage.BootIndexOptions) (digest.Digest, error) {
		if opts.RootfsArchivePath == "" || opts.SandboxArchivePath == "" || opts.Tag == "" {
			t.Fatalf("boot options = %#v", opts)
		}
		return digest.FromString("boot"), os.WriteFile(opts.ArchivePath, []byte("boot-archive"), 0o644)
	}

	imgSvc := &fakeImageService{}
	snapSvc := &fakeSnapshotService{}
	server := newConvertHandlerServer(imgSvc, snapSvc, nil)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("metadata", `{"source":"docker.io/library/nginx:latest","namespace":"team-a","boot_index_tag":"localhost/conch/demo:latest","plain_http":true}`)
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
	if snapSvc.linkReq.RootfsSnapshotID != "rootfs-id" || snapSvc.linkReq.VMSnapshotID == "" || snapSvc.linkReq.Namespace != "team-a" {
		t.Fatalf("link request = %#v", snapSvc.linkReq)
	}
	if len(imgSvc.importReqs) != 2 || imgSvc.importReqs[1].ImportedTag != "localhost/conch/demo:latest" {
		t.Fatalf("import requests = %#v", imgSvc.importReqs)
	}
	var got convertImageResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.BootIndexTag != "localhost/conch/demo:latest" || got.BootIndexDigest == "" || got.RootfsImageRef == "" {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleSnapshotExport(t *testing.T) {
	oldBoot := buildBootIndexArchive
	defer func() { buildBootIndexArchive = oldBoot }()
	buildBootIndexArchive = func(_ context.Context, opts conchimage.BootIndexOptions) (digest.Digest, error) {
		if len(opts.MemChainPaths) == 0 || len(opts.SandboxChainPaths) == 0 || opts.Tag == "" {
			t.Fatalf("boot options = %#v", opts)
		}
		return digest.FromString("snapshot"), os.WriteFile(opts.ArchivePath, []byte("snapshot-archive"), 0o644)
	}

	imgSvc := &fakeImageService{}
	snapSvc := &fakeSnapshotService{}
	manager := &fakeSandboxOps{}
	server := newConvertHandlerServer(imgSvc, snapSvc, manager)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/export", bytes.NewBufferString(`{"sandbox_id":"sandbox-123","namespace":"team-a","boot_index_tag":"localhost/conch/snap:latest"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if manager.pauseReq.SandboxId != "sandbox-123" || manager.pauseReq.Namespace != "team-a" {
		t.Fatalf("pause request = %#v", manager.pauseReq)
	}
	if snapSvc.infoReq.Key != "paused-rootfs-id" || snapSvc.chainReq.Namespace != "team-a" {
		t.Fatalf("snapshot requests info=%#v chain=%#v", snapSvc.infoReq, snapSvc.chainReq)
	}
	if imgSvc.exportReq.ImageName != "rootfs-image:latest" || imgSvc.exportReq.Namespace != "team-a" {
		t.Fatalf("export request = %#v", imgSvc.exportReq)
	}
	if len(imgSvc.importReqs) != 1 || imgSvc.importReqs[0].ImportedTag != "localhost/conch/snap:latest" {
		t.Fatalf("import requests = %#v", imgSvc.importReqs)
	}
	var got snapshotExportResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.BootIndexTag != "localhost/conch/snap:latest" || got.BootIndexDigest == "" {
		t.Fatalf("response = %#v", got)
	}
}

func TestHandleListAndRemoveSnapshot(t *testing.T) {
	svc := &fakeSnapshotService{
		snapshots: []snapshotSvc.Meta{{
			Key:    "sha256:rootfs",
			Kind:   "committed",
			Parent: "sha256:parent",
		}},
	}
	server := newSnapshotHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/list", bytes.NewBufferString(`{"namespace":"team-a","filters":["kind==committed"]}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.listReq.Namespace != "team-a" || len(svc.listReq.Filters) != 1 {
		t.Fatalf("list request = %#v", svc.listReq)
	}
	var listResp struct {
		Snapshots []snapshotSvc.Meta `json:"snapshots"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Snapshots) != 1 || listResp.Snapshots[0].Key != "sha256:rootfs" {
		t.Fatalf("list response = %#v", listResp)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/snapshot/remove", bytes.NewBufferString(`{"namespace":"team-a","key":"sha256:rootfs","cascade":true}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.removeReq.Namespace != "team-a" || svc.removeReq.Key != "sha256:rootfs" || !svc.removeReq.Cascade {
		t.Fatalf("remove request = %#v", svc.removeReq)
	}
}

func TestHandleRemoveSnapshotInvalidRequest(t *testing.T) {
	svc := &fakeSnapshotService{
		removeErr: errors.Join(snapshotSvc.ErrInvalidRequest, errors.New("key is required")),
	}
	server := newSnapshotHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/remove", bytes.NewBufferString(`{"namespace":"team-a"}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("remove status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if svc.removeReq.Namespace != "team-a" || svc.removeReq.Key != "" {
		t.Fatalf("remove request = %#v", svc.removeReq)
	}
}

func fmtInvalidImageName() error {
	return errors.Join(imageSvc.ErrInvalidRequest, errors.New("image_name is required"))
}
