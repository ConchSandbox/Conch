package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
)

type fakeImageService struct {
	pullReq               runtimeapi.PullImageOptions
	pushReq               runtimeapi.PushImageOptions
	listReq               runtimeapi.ListImagesOptions
	removeReq             runtimeapi.RemoveImageOptions
	unpackReq             runtimeapi.UnpackImageOptions
	prepareReq            conchimage.PrepareRootfsSourceOptions
	convertReq            erofsconvert.ConvertRootfsRequest
	publishReq            conchimage.PublishBootImageOptions
	inspectReq            bootIndexCall
	inspectReferenceReq   bootIndexReferenceCall
	pushBootIndexReq      conchimage.PushBootIndexOptions
	checkpointPublishReq  conchimage.PublishCheckpointBootImageOptions
	exportReq             runtimeapi.ExportImageArchiveOptions
	pullErr               error
	pushErr               error
	listErr               error
	removeErr             error
	unpackErr             error
	prepareErr            error
	convertErr            error
	publishErr            error
	inspectErr            error
	inspectReferenceErr   error
	checkpointPublishErr  error
	exportErr             error
	results               map[string]string
	importReqs            []runtimeapi.ImportImageArchiveOptions
	importRaws            []string
	exportRaw             string
	prepareResp           conchimage.PrepareRootfsSourceResult
	convertResp           erofsconvert.ConvertRootfsResult
	images                []runtimeapi.ImageRecord
	publishResp           conchimage.PublishBootImageResult
	inspectResp           conchimage.BootIndexInfo
	inspectReferenceResp  conchimage.BootIndexInfo
	checkpointPublishResp conchimage.PublishCheckpointBootImageResult
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
	createReq      sandbox.CreateRequest
	checkpointReq  sandbox.CheckpointRequest
	suspendReq     sandbox.LifecycleRequest
	resumeReq      sandbox.LifecycleRequest
	updateReq      sandbox.NetworkUpdateRequest
	deleteReqs     []sandbox.DeleteRequest
	createErr      error
	checkpointErr  error
	checkpointResp sandbox.CheckpointResult
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

func (f *fakeImageService) PushBootIndex(_ context.Context, req conchimage.PushBootIndexOptions) error {
	f.pushBootIndexReq = req
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
		ImageName:       req.BootIndexTag,
	}, nil
}

type bootIndexCall struct {
	Namespace       string
	BootIndexDigest string
}

type bootIndexReferenceCall struct {
	Namespace string
	Reference string
}

func (f *fakeImageService) InspectBootIndex(_ context.Context, namespace, bootIndexDigest string) (conchimage.BootIndexInfo, error) {
	f.inspectReq = bootIndexCall{Namespace: namespace, BootIndexDigest: bootIndexDigest}
	return f.inspectResp, f.inspectErr
}

func (f *fakeImageService) InspectBootIndexReference(_ context.Context, namespace, reference string) (conchimage.BootIndexInfo, error) {
	f.inspectReferenceReq = bootIndexReferenceCall{Namespace: namespace, Reference: reference}
	return f.inspectReferenceResp, f.inspectReferenceErr
}

func (f *fakeImageService) PublishCheckpointBootImage(_ context.Context, req conchimage.PublishCheckpointBootImageOptions) (conchimage.PublishCheckpointBootImageResult, error) {
	f.checkpointPublishReq = req
	if f.checkpointPublishErr != nil {
		return conchimage.PublishCheckpointBootImageResult{}, f.checkpointPublishErr
	}
	if f.checkpointPublishResp.BootIndexDigest != "" {
		return f.checkpointPublishResp, nil
	}
	return conchimage.PublishCheckpointBootImageResult{
		BootIndexDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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

func newRuntimeForTest(image conchruntime.ImageOps, templateBootIndex conchruntime.TemplateBootIndexOps, snapshot conchruntime.SnapshotOps, sandboxOps conchruntime.SandboxOps) *conchruntime.Service {
	rt := conchruntime.New(sandboxOps, image, templateBootIndex, nil, "default")
	rt.Snapshot = snapshot
	return rt
}

func newImageHandlerServer(svc conchruntime.ImageOps) *Daemon {
	s := &Daemon{
		router:         http.NewServeMux(),
		runtimeService: newRuntimeForTest(svc, nil, nil, nil),
	}
	s.routes()
	return s
}

func newSnapshotHandlerServer(svc conchruntime.SnapshotOps) *Daemon {
	s := &Daemon{
		router:         http.NewServeMux(),
		runtimeService: newRuntimeForTest(nil, nil, svc, nil),
	}
	s.routes()
	return s
}

func newConvertHandlerServer(imageSvc conchruntime.ImageOps, templateBootIndexSvc conchruntime.TemplateBootIndexOps, snapshotSvc conchruntime.SnapshotOps, sandboxOps conchruntime.SandboxOps) *Daemon {
	s := &Daemon{
		router:         http.NewServeMux(),
		runtimeService: newRuntimeForTest(imageSvc, templateBootIndexSvc, snapshotSvc, sandboxOps),
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

func (f *fakeSandboxOps) Create(req sandbox.CreateRequest) (sandbox.CreateResult, error) {
	f.createReq = req
	if f.createErr != nil {
		return sandbox.CreateResult{}, f.createErr
	}
	return sandbox.CreateResult{
		IP:         "192.0.2.2",
		AgentToken: req.AgentToken,
		Namespace:  req.Namespace,
		SandboxID:  req.SandboxID,
	}, nil
}

func (f *fakeSandboxOps) Delete(req sandbox.DeleteRequest) error {
	f.deleteReqs = append(f.deleteReqs, req)
	return nil
}

func (f *fakeSandboxOps) Suspend(req sandbox.LifecycleRequest) error {
	f.suspendReq = req
	return nil
}

func (f *fakeSandboxOps) Resume(req sandbox.LifecycleRequest) error {
	f.resumeReq = req
	return nil
}

func (f *fakeSandboxOps) UpdateNetwork(req sandbox.NetworkUpdateRequest) error {
	f.updateReq = req
	return nil
}

func (f *fakeSandboxOps) Checkpoint(req sandbox.CheckpointRequest) (sandbox.CheckpointResult, error) {
	f.checkpointReq = req
	if f.checkpointErr != nil {
		return sandbox.CheckpointResult{}, f.checkpointErr
	}
	if f.checkpointResp.MemRootPath != "" {
		return f.checkpointResp, nil
	}
	memRoot, err := os.MkdirTemp("", "conch-daemon-checkpoint-test-*")
	if err != nil {
		return sandbox.CheckpointResult{}, err
	}
	return sandbox.CheckpointResult{
		MemRootPath: memRoot,
		VMMName:     "cloud-hypervisor",
	}, nil
}

func TestHandleHealth(t *testing.T) {
	store, err := state.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ready := &Daemon{
		stateStore:     store,
		containerdHost: &containerdhost.Host{},
		daemonClient:   &containerdclient.Client{},
		runtimeService: &conchruntime.Service{Sandbox: &fakeSandboxOps{}, Store: store},
	}
	for _, test := range []struct {
		name   string
		daemon *Daemon
		want   int
	}{
		{name: "not ready", daemon: &Daemon{}, want: http.StatusServiceUnavailable},
		{name: "ready", daemon: ready, want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.daemon.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
		})
	}
}

func TestMatchesSandboxState(t *testing.T) {
	for _, test := range []struct {
		state  string
		states map[string]bool
		want   bool
	}{
		{state: state.SandboxReady, want: true},
		{state: state.SandboxSuspended, want: true},
		{state: state.SandboxStopped, want: true},
		{state: state.SandboxUnknown, want: false},
		{state: state.SandboxStopped, states: map[string]bool{"paused": true}, want: true},
		{state: state.SandboxStopped, states: map[string]bool{"running": true}, want: false},
	} {
		if got := matchesSandboxState(state.SandboxRecord{State: test.state}, test.states); got != test.want {
			t.Fatalf("matchesSandboxState(%q, %v) = %v, want %v", test.state, test.states, got, test.want)
		}
	}
}

func TestSandboxNetworkResponseFromRecordPreservesPresence(t *testing.T) {
	maskRequestHost := "masked.example"
	for _, test := range []struct {
		name          string
		allowInternet *bool
	}{
		{name: "unset"},
		{name: "false", allowInternet: testBoolPointer(false)},
		{name: "true", allowInternet: testBoolPointer(true)},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(runtimeapi.SandboxNetworkConfig{
				AllowOut:            []string{"192.0.2.1"},
				MaskRequestHost:     &maskRequestHost,
				AllowInternetAccess: test.allowInternet,
			})
			if err != nil {
				t.Fatal(err)
			}
			network, allowInternet := sandboxNetworkResponseFromRecord(raw)
			if network.MaskRequestHost != maskRequestHost ||
				!reflect.DeepEqual(network.AllowOut, []string{"192.0.2.1"}) {
				t.Fatalf("network response = %#v", network)
			}
			if !reflect.DeepEqual(allowInternet, test.allowInternet) {
				t.Fatalf("allowInternetAccess = %#v, want %#v", allowInternet, test.allowInternet)
			}
		})
	}
}

func testBoolPointer(value bool) *bool {
	return &value
}

func TestSandboxV1Handlers(t *testing.T) {
	store, err := state.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sandboxOps := &fakeSandboxOps{}
	runtimeService := conchruntime.New(sandboxOps, nil, nil, store, "default")
	runtimeService.SetSandboxDefaults(runtimeapi.SandboxDefaults{
		TemplateID: "tmpl-default",
		VCPUNum:    4,
		VCPUMax:    4,
		RamMB:      256,
	})
	server := &Daemon{
		router:         http.NewServeMux(),
		stateStore:     store,
		runtimeService: runtimeService,
	}
	server.routes()

	allowInternet := false
	maskRequestHost := "masked.example"
	network, err := json.Marshal(runtimeapi.SandboxNetworkConfig{
		AllowOut:            []string{"192.0.2.1"},
		MaskRequestHost:     &maskRequestHost,
		AllowInternetAccess: &allowInternet,
	})
	if err != nil {
		t.Fatalf("marshal network: %v", err)
	}
	if err := store.UpsertSandbox(context.Background(), state.SandboxRecord{
		PodSandboxID:     "pod-1",
		ConchSandboxID:   "sandbox-1",
		Namespace:        "default",
		State:            state.SandboxReady,
		SourceTemplateID: "tmpl-1",
		VCPUNum:          2,
		RamMB:            128,
		Network:          network,
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	t.Run("list", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodGet, "/api/v1/sandboxes?limit=1", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var records []sandboxResponse
		if err := json.NewDecoder(response.Body).Decode(&records); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
		if len(records) != 1 || records[0].SandboxID != "sandbox-1" || records[0].TemplateID != "tmpl-1" {
			t.Fatalf("list response = %#v", records)
		}
	})

	t.Run("get", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodGet, "/api/v1/sandboxes/sandbox-1", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var record sandboxResponse
		if err := json.NewDecoder(response.Body).Decode(&record); err != nil {
			t.Fatalf("decode get response: %v", err)
		}
		if record.SandboxID != "sandbox-1" || record.TemplateID != "tmpl-1" ||
			record.ConchInitAccessToken == nil || record.AllowInternetAccess == nil || *record.AllowInternetAccess ||
			record.Domain == nil || record.Network == nil ||
			record.Network.MaskRequestHost != maskRequestHost ||
			!reflect.DeepEqual(record.Network.AllowOut, []string{"192.0.2.1"}) {
			t.Fatalf("get response = %#v", record)
		}
	})

	t.Run("create", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{
			"sandbox_id":"sandbox-2","template_id":"tmpl-2","env":{"SOME_RANDOM_KEY":"key123"}
		}`))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var record createSandboxResponse
		if err := json.NewDecoder(response.Body).Decode(&record); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
		if record.SandboxID != "sandbox-2" || record.TemplateID != "tmpl-2" ||
			record.Domain != "192.0.2.2" || record.ConchInitAccessToken == "" {
			t.Fatalf("create response = %#v", record)
		}
		if got := sandboxOps.createReq.Env["SOME_RANDOM_KEY"]; got != "key123" {
			t.Fatalf("Env[SOME_RANDOM_KEY] = %q, want key123", got)
		}
	})

	t.Run("create rejects unsupported network fields", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{
			"sandbox_id":"sandbox-invalid","template_id":"tmpl-2","network":{"rules":{"rule":true}}
		}`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	})

	t.Run("logs", func(t *testing.T) {
		runtimeService.AppendSandboxLog("sandbox-1", "info", "created sandbox")
		runtimeService.AppendSandboxLog("sandbox-1", "error", "network update failed")
		response := serveSandboxRequest(server, http.MethodGet, "/api/v1/sandboxes/sandbox-1/logs?limit=1&direction=backward&level=info&search=CREATED", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var result getSandboxLogsResponse
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatalf("decode logs response: %v", err)
		}
		if len(result.Logs) != 1 || result.Logs[0].Message != "created sandbox" ||
			result.Logs[0].Level != "info" || result.Logs[0].Fields["sandboxID"] != "sandbox-1" ||
			result.Logs[0].Fields["namespace"] != "default" || result.NextCursor == "" {
			t.Fatalf("logs response = %#v", result)
		}
		wrongNamespace := serveSandboxRequest(server, http.MethodGet, "/api/v1/sandboxes/sandbox-1/logs?namespace=other", nil)
		if wrongNamespace.Code != http.StatusNotFound {
			t.Fatalf("wrong namespace status = %d, body = %s", wrongNamespace.Code, wrongNamespace.Body.String())
		}
	})

	t.Run("network", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodPut, "/api/v1/sandboxes/sandbox-1/network", bytes.NewBufferString(`{"allowOut":["192.0.2.1"]}`))
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if sandboxOps.updateReq.SandboxID != "sandbox-1" || len(sandboxOps.updateReq.Network) == 0 {
			t.Fatalf("network update request = %#v", sandboxOps.updateReq)
		}
	})

	t.Run("network accepts empty reserved fields", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodPut, "/api/v1/sandboxes/sandbox-1/network", bytes.NewBufferString(`{"egressProxy":{},"rules":{}}`))
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	})

	t.Run("network rejects unsupported proxy", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodPut, "/api/v1/sandboxes/sandbox-1/network", bytes.NewBufferString(`{"egressProxy":{"username":"user"}}`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	})

	t.Run("delete", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodDelete, "/api/v1/sandboxes/sandbox-1", nil)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if len(sandboxOps.deleteReqs) != 1 || sandboxOps.deleteReqs[0].SandboxId != "sandbox-1" {
			t.Fatalf("delete requests = %#v", sandboxOps.deleteReqs)
		}
		if _, err := store.GetSandbox(context.Background(), "pod-1"); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("deleted sandbox lookup error = %v", err)
		}
	})
}

func serveSandboxRequest(server *Daemon, method, path string, body io.Reader) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.router.ServeHTTP(recorder, httptest.NewRequest(method, path, body))
	return recorder
}
