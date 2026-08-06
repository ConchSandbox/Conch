package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
)

type fakeSnapshotService struct {
	listReq     runtimeapi.ListSnapshotsOptions
	removeReq   runtimeapi.RemoveSnapshotOptions
	infoReq     runtimeapi.SnapshotInfoOptions
	listErr     error
	removeErr   error
	infoErr     error
	removeCalls int
	snapshots   []runtimeapi.SnapshotRecord
	infoResp    runtimeapi.SnapshotRecord
}

type fakeSandboxOps struct {
	createReq      sandbox.CreateRequest
	checkpointReq  sandbox.CheckpointRequest
	suspendReq     sandbox.LifecycleRequest
	resumeReq      sandbox.LifecycleRequest
	deleteReqs     []sandbox.DeleteRequest
	createErr      error
	checkpointErr  error
	checkpointResp sandbox.CheckpointResult
}

func newSnapshotHandlerServer(svc conchruntime.SnapshotOps) *Daemon {
	runtimeService := conchruntime.New(nil, nil, nil)
	runtimeService.Snapshot = svc
	s := &Daemon{
		router:         http.NewServeMux(),
		runtimeService: runtimeService,
	}
	s.routes()
	return s
}

func (f *fakeSnapshotService) List(_ context.Context, req runtimeapi.ListSnapshotsOptions) ([]runtimeapi.SnapshotRecord, error) {
	f.listReq = req
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.snapshots, nil
}

func (f *fakeSnapshotService) Remove(_ context.Context, req runtimeapi.RemoveSnapshotOptions) error {
	f.removeCalls++
	f.removeReq = req
	return f.removeErr
}

func (f *fakeSnapshotService) Info(_ context.Context, req runtimeapi.SnapshotInfoOptions) (runtimeapi.SnapshotRecord, error) {
	f.infoReq = req
	if f.infoErr != nil {
		return runtimeapi.SnapshotRecord{}, f.infoErr
	}
	if f.infoResp.Key != "" {
		return f.infoResp, nil
	}
	return runtimeapi.SnapshotRecord{
		Key:         req.Key,
		Parent:      "parent-id",
		StoragePath: "/snap/rootfs",
	}, nil
}

func (f *fakeSandboxOps) Create(req sandbox.CreateRequest) (sandbox.CreateResult, error) {
	f.createReq = req
	if f.createErr != nil {
		return sandbox.CreateResult{}, f.createErr
	}
	return sandbox.CreateResult{
		IP:              "192.0.2.2",
		AgentToken:      req.AgentToken,
		SandboxID:       req.SandboxID,
		BootIndexDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
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
		{state: state.SandboxUnknown, want: false},
		{state: state.SandboxSuspended, states: map[string]bool{"paused": true}, want: true},
		{state: state.SandboxSuspended, states: map[string]bool{"running": true}, want: false},
		{state: state.SandboxReady, states: map[string]bool{"running": true}, want: true},
		{state: state.SandboxReady, states: map[string]bool{"paused": true}, want: false},
	} {
		if got := matchesSandboxState(state.SandboxRecord{State: test.state}, test.states); got != test.want {
			t.Fatalf("matchesSandboxState(%q, %v) = %v, want %v", test.state, test.states, got, test.want)
		}
	}
}

func TestParseSandboxStates(t *testing.T) {
	got, err := parseSandboxStates([]string{"running", "paused"})
	if err != nil || !got["running"] || !got["paused"] {
		t.Fatalf("parseSandboxStates() = %v, %v", got, err)
	}
	if _, err := parseSandboxStates([]string{"stopped"}); err == nil {
		t.Fatal("parseSandboxStates() accepted unsupported state")
	}
	if got, err := parseSandboxStates(nil); err != nil || got != nil {
		t.Fatalf("parseSandboxStates(nil) = %v, %v", got, err)
	}
}

func TestParseSandboxListLimit(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int
		ok   bool
	}{
		{raw: "", want: 100, ok: true},
		{raw: "1", want: 1, ok: true},
		{raw: "5000", want: 5000, ok: true},
		{raw: "0"},
		{raw: "5001"},
		{raw: "invalid"},
	} {
		got, err := parseSandboxListLimit(test.raw)
		if test.ok && (err != nil || got != test.want) {
			t.Fatalf("parseSandboxListLimit(%q) = %d, %v; want %d", test.raw, got, err, test.want)
		}
		if !test.ok && err == nil {
			t.Fatalf("parseSandboxListLimit(%q) unexpectedly succeeded", test.raw)
		}
	}
}

func TestSandboxV1Handlers(t *testing.T) {
	store, err := state.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sandboxOps := &fakeSandboxOps{}
	runtimeService := conchruntime.New(sandboxOps, nil, store)
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

	if err := store.UpsertSandbox(context.Background(), state.SandboxRecord{
		SandboxID:                     "sandbox-1",
		State:                         state.SandboxReady,
		SourceTemplateID:              "tmpl-1",
		CheckpointHeadTemplateID:      "tmpl-1",
		CheckpointHeadBootIndexDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		VCPUNum:                       2,
		RamMB:                         128,
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	t.Run("list", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodGet, "/api/v1/sandboxes?limit=1", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var records []sandboxInspectResponse
		if err := json.NewDecoder(response.Body).Decode(&records); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
		if len(records) != 1 || records[0].SandboxID != "sandbox-1" || records[0].TemplateID != "tmpl-1" {
			t.Fatalf("list response = %#v", records)
		}
	})

	t.Run("rejects invalid list queries", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/sandboxes?state=stopped",
			"/api/v1/sandboxes?limit=0",
			"/api/v1/sandboxes?limit=5001",
		} {
			response := serveSandboxRequest(server, http.MethodGet, path, nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("%s status = %d, body = %s", path, response.Code, response.Body.String())
			}
		}
	})

	t.Run("get", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodGet, "/api/v1/sandboxes/sandbox-1", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var record sandboxInspectResponse
		if err := json.NewDecoder(response.Body).Decode(&record); err != nil {
			t.Fatalf("decode get response: %v", err)
		}
		if record.SandboxID != "sandbox-1" || record.TemplateID != "tmpl-1" || record.Domain == nil {
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

	t.Run("rejects unknown sandbox subroute", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodGet, "/api/v1/sandboxes/sandbox-1/unknown", nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	})

	t.Run("delete", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodDelete, "/api/v1/sandboxes/sandbox-1", nil)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if len(sandboxOps.deleteReqs) != 1 || sandboxOps.deleteReqs[0].SandboxID != "sandbox-1" {
			t.Fatalf("delete requests = %#v", sandboxOps.deleteReqs)
		}
		if _, err := store.GetSandbox(context.Background(), "sandbox-1"); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("deleted sandbox lookup error = %v", err)
		}
	})
}

func serveSandboxRequest(server *Daemon, method, path string, body io.Reader) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.router.ServeHTTP(recorder, httptest.NewRequest(method, path, body))
	return recorder
}
