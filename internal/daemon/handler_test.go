package daemon

import (
	"context"
	"io"
	"net/http"

	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	"github.com/openeuler/Conch/internal/conchruntime"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
)

type fakeImageService struct {
	pullReq     runtimeapi.PullImageOptions
	pushReq     runtimeapi.PushImageOptions
	listReq     runtimeapi.ListImagesOptions
	removeReq   runtimeapi.RemoveImageOptions
	unpackReq   runtimeapi.UnpackImageOptions
	prepareReq  conchimage.PrepareRootfsSourceOptions
	convertReq  erofsconvert.ConvertRootfsRequest
	publishReq  conchimage.PublishBootImageOptions
	exportReq   runtimeapi.ExportImageArchiveOptions
	pullErr     error
	pushErr     error
	listErr     error
	removeErr   error
	unpackErr   error
	prepareErr  error
	convertErr  error
	publishErr  error
	exportErr   error
	results     map[string]string
	importReqs  []runtimeapi.ImportImageArchiveOptions
	importRaws  []string
	exportRaw   string
	prepareResp conchimage.PrepareRootfsSourceResult
	convertResp erofsconvert.ConvertRootfsResult
	images      []runtimeapi.ImageRecord
	publishResp conchimage.PublishBootImageResult
}

type fakeSnapshotService struct {
	listReq   snapshotSvc.ListRequest
	removeReq snapshotSvc.RemoveRequest
	infoReq   snapshotSvc.InfoRequest
	chainReq  snapshotSvc.InfoRequest
	listErr   error
	removeErr error
	infoErr   error
	chainErr  error
	snapshots []snapshotSvc.Meta
}

type fakeSandboxOps struct {
	createReq     sandbox.SandboxCreateRequest
	checkpointReq sandbox.SandboxCheckpointRequest
	suspendReq    sandbox.SandboxLifecycleRequest
	resumeReq     sandbox.SandboxLifecycleRequest
	deleteReqs    []sandbox.SandboxDeleteRequest
	createErr     error
	checkpointErr error
}

func (f *fakeImageService) Pull(_ context.Context, req runtimeapi.PullImageOptions) (runtimeapi.PullImageResult, error) {
	f.pullReq = req
	if f.pullErr != nil {
		return runtimeapi.PullImageResult{}, f.pullErr
	}
	return runtimeapi.PullImageResult{Refs: f.results}, nil
}

func (f *fakeImageService) Push(_ context.Context, req runtimeapi.PushImageOptions) error {
	f.pushReq = req
	return f.pushErr
}

func (f *fakeImageService) List(_ context.Context, req runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error) {
	f.listReq = req
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.images, nil
}

func (f *fakeImageService) Remove(_ context.Context, req runtimeapi.RemoveImageOptions) error {
	f.removeReq = req
	return f.removeErr
}

func (f *fakeImageService) Unpack(_ context.Context, req runtimeapi.UnpackImageOptions) (map[string]string, error) {
	f.unpackReq = req
	if f.unpackErr != nil {
		return nil, f.unpackErr
	}
	return f.results, nil
}

func (f *fakeImageService) ImportArchive(_ context.Context, archive io.Reader, req runtimeapi.ImportImageArchiveOptions) (runtimeapi.ImportImageArchiveResult, error) {
	f.importReqs = append(f.importReqs, req)
	raw, _ := io.ReadAll(archive)
	f.importRaws = append(f.importRaws, string(raw))
	return runtimeapi.ImportImageArchiveResult{SnapshotKey: req.ImportedTag + "-snapshot", ImageName: req.ImportedTag}, nil
}

func (f *fakeImageService) ExportArchive(_ context.Context, w io.Writer, req runtimeapi.ExportImageArchiveOptions) error {
	f.exportReq = req
	if f.exportErr != nil {
		return f.exportErr
	}
	_, _ = io.WriteString(w, "archive-content")
	f.exportRaw = "archive-content"
	return nil
}

func (f *fakeImageService) PublishBootImage(_ context.Context, req conchimage.PublishBootImageOptions) (conchimage.PublishBootImageResult, error) {
	f.publishReq = req
	if f.publishErr != nil {
		return conchimage.PublishBootImageResult{}, f.publishErr
	}
	if f.publishResp.ImageName != "" {
		return f.publishResp, nil
	}
	return conchimage.PublishBootImageResult{
		BootIndexDigest: "sha256:boot",
		RootfsKey:       "rootfs-id",
		VMKey:           "vm-id",
		ImageName:       req.BootIndexTag,
	}, nil
}

func (f *fakeImageService) PrepareRootfsSource(_ context.Context, req conchimage.PrepareRootfsSourceOptions) (conchimage.PrepareRootfsSourceResult, error) {
	f.prepareReq = req
	if f.prepareErr != nil {
		return conchimage.PrepareRootfsSourceResult{}, f.prepareErr
	}
	if f.prepareResp.ImageName != "" {
		return f.prepareResp, nil
	}
	imageName := req.Source
	if req.TargetImage != "" {
		imageName = req.TargetImage
	}
	return conchimage.PrepareRootfsSourceResult{
		ImageName:      imageName,
		ManifestDigest: "sha256:manifest",
	}, nil
}

func (f *fakeImageService) ConvertRootfsToErofs(_ context.Context, req erofsconvert.ConvertRootfsRequest) (erofsconvert.ConvertRootfsResult, error) {
	f.convertReq = req
	if f.convertErr != nil {
		return erofsconvert.ConvertRootfsResult{}, f.convertErr
	}
	if f.convertResp.ImageName != "" {
		return f.convertResp, nil
	}
	return erofsconvert.ConvertRootfsResult{
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

func (f *fakeSandboxOps) Create(req sandbox.SandboxCreateRequest) (sandbox.SandboxCreateResult, error) {
	f.createReq = req
	if f.createErr != nil {
		return sandbox.SandboxCreateResult{}, f.createErr
	}
	return sandbox.SandboxCreateResult{IP: "192.0.2.2", Namespace: req.Namespace, SandboxID: req.SandboxId}, nil
}

func (f *fakeSandboxOps) Delete(req sandbox.SandboxDeleteRequest) error {
	f.deleteReqs = append(f.deleteReqs, req)
	return nil
}

func (f *fakeSandboxOps) Suspend(req sandbox.SandboxLifecycleRequest) error {
	f.suspendReq = req
	return nil
}

func (f *fakeSandboxOps) Resume(req sandbox.SandboxLifecycleRequest) error {
	f.resumeReq = req
	return nil
}

func (f *fakeSandboxOps) Checkpoint(req sandbox.SandboxCheckpointRequest) (sandbox.SandboxCheckpointResult, error) {
	f.checkpointReq = req
	if f.checkpointErr != nil {
		return sandbox.SandboxCheckpointResult{}, f.checkpointErr
	}
	return sandbox.SandboxCheckpointResult{RootfsKey: "paused-rootfs-id", MemKey: "mem-id", VMKey: "vm-id"}, nil
}
