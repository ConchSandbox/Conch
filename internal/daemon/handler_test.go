package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/openeuler/Conch/internal/id"
	"github.com/openeuler/Conch/internal/netstack"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/internal/conchruntime"
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
	store          *memorySandboxStore
	createReq      sandbox.CreateRequest
	checkpointReq  string
	suspendReq     string
	resumeReq      string
	deleteReqs     []string
	createErr      error
	createCalls    int
	checkpointErr  error
	checkpointResp sandbox.CheckpointResult
	updateReq      sandbox.NetworkUpdateRequest
}

func newSnapshotHandlerServer(svc conchruntime.SnapshotOps) *Daemon {
	runtimeService := newHandlerRuntime(nil, nil, nil)
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

func newHandlerRuntime(ops conchruntime.SandboxOps, client *containerdclient.Client, store sandbox.Store) *conchruntime.Service {
	if fake, ok := ops.(*fakeSandboxOps); ok {
		fake.store, _ = store.(*memorySandboxStore)
	}
	return conchruntime.New(ops, client)
}

func (f *fakeSandboxOps) Create(ctx context.Context, req sandbox.CreateRequest) (runtimeapi.SandboxCreateResult, error) {
	f.createCalls++
	if f.createErr != nil {
		return runtimeapi.SandboxCreateResult{}, f.createErr
	}
	if req.SandboxID == "" {
		req.SandboxID, _ = id.New()
	}
	if err := id.Validate(req.SandboxID); err != nil {
		return runtimeapi.SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(err)
	}
	if err := agentprotocol.ValidateEnvironment(req.Env); err != nil {
		return runtimeapi.SandboxCreateResult{}, sandbox.ErrInvalidEnvironment.Wrap(err)
	}
	if err := netstack.ValidateSandboxNetworkInputConfig(ctx, req.Network); err != nil {
		return runtimeapi.SandboxCreateResult{}, err
	}
	if req.VCPUNum < 1 || req.RAMMB < 1 {
		return runtimeapi.SandboxCreateResult{}, sandbox.ErrInvalidArgument.New()
	}
	req.AgentToken, _ = sandbox.GenerateAgentToken()
	f.createReq = req
	result := runtimeapi.SandboxCreateResult{
		IP: "192.0.2.2", AgentToken: req.AgentToken, SandboxID: req.SandboxID,
		TemplateID: req.TemplateID, TemplateName: req.TemplateName, VCPUNum: req.VCPUNum, RamMB: req.RAMMB, CreatedAt: time.Now().UnixNano(),
	}
	if f.store != nil {
		_, err := f.store.Create(ctx, sandbox.Record{ID: req.SandboxID, State: sandbox.StateReady,
			SourceTemplateName: req.TemplateName, SourceTemplateID: req.TemplateID,
			CheckpointHeadTemplateID: req.TemplateID, CreatedAt: result.CreatedAt,
			IP: result.IP, VCPUNum: req.VCPUNum, RamMB: req.RAMMB, Network: req.Network})
		if err != nil {
			return runtimeapi.SandboxCreateResult{}, err
		}
	}
	return result, nil
}
func (f *fakeSandboxOps) Delete(ctx context.Context, sandboxID string) error {
	f.deleteReqs = append(f.deleteReqs, sandboxID)
	if f.store != nil {
		return f.store.Delete(ctx, sandboxID)
	}
	return nil
}
func (f *fakeSandboxOps) Suspend(ctx context.Context, sandboxID string) error {
	f.suspendReq = sandboxID
	return f.setState(ctx, sandboxID, sandbox.StateSuspended)
}
func (f *fakeSandboxOps) Resume(ctx context.Context, sandboxID string) error {
	f.resumeReq = sandboxID
	return f.setState(ctx, sandboxID, sandbox.StateReady)
}
func (f *fakeSandboxOps) setState(ctx context.Context, sandboxID string, state sandbox.State) error {
	if f.store == nil {
		return nil
	}
	rec, err := f.store.Get(ctx, sandboxID)
	if err != nil {
		return err
	}
	rec.State = state
	_, err = f.store.Update(ctx, rec)
	return err
}
func (f *fakeSandboxOps) UpdateNetwork(ctx context.Context, req sandbox.NetworkUpdateRequest) error {
	if err := netstack.ValidateSandboxNetworkInputConfig(ctx, req.Network); err != nil {
		return err
	}
	f.updateReq = req
	if f.store == nil {
		return nil
	}
	rec, err := f.store.Get(ctx, req.SandboxID)
	if err != nil {
		return err
	}
	rec.Network = req.Network
	_, err = f.store.Update(ctx, rec)
	return err
}
func (f *fakeSandboxOps) Checkpoint(ctx context.Context, sandboxID string, register func(context.Context, sandbox.CheckpointResult) error) (sandbox.CheckpointResult, error) {
	f.checkpointReq = sandboxID
	if f.checkpointErr != nil {
		return sandbox.CheckpointResult{}, f.checkpointErr
	}
	return f.checkpointResp, register(ctx, f.checkpointResp)
}

func TestHandleHealth(t *testing.T) {
	store := newMemorySandboxStore()
	ready := &Daemon{
		sandboxStore:   store,
		containerdHost: &containerdhost.Host{},
		daemonClient:   &containerdclient.Client{},
		runtimeService: &conchruntime.Service{Sandbox: &fakeSandboxOps{store: store}},
	}
	for _, test := range []struct {
		name     string
		daemon   *Daemon
		method   string
		want     int
		wantCode string
	}{
		{name: "not ready", daemon: &Daemon{}, method: http.MethodGet, want: http.StatusServiceUnavailable, wantCode: "service.unavailable"},
		{name: "ready", daemon: ready, method: http.MethodGet, want: http.StatusNoContent},
		{name: "method not allowed", daemon: ready, method: http.MethodPost, want: http.StatusMethodNotAllowed, wantCode: "request.method_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.daemon.handleHealth(recorder, httptest.NewRequest(test.method, "/health", nil))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
			if test.wantCode == "" {
				if recorder.Body.Len() != 0 {
					t.Fatalf("body = %q, want empty", recorder.Body.String())
				}
				return
			}
			var apiErr apiErrorResponse
			if err := json.NewDecoder(recorder.Body).Decode(&apiErr); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if string(apiErr.Code) != test.wantCode {
				t.Fatalf("code = %q, want %q", apiErr.Code, test.wantCode)
			}
		})
	}
}

func TestMatchesSandboxState(t *testing.T) {
	for _, test := range []struct {
		state  sandbox.State
		states map[string]bool
		want   bool
	}{
		{state: sandbox.StateReady, want: true},
		{state: sandbox.StateSuspended, want: true},
		{state: sandbox.StateUnknown, want: false},
		{state: sandbox.StateSuspended, states: map[string]bool{"paused": true}, want: true},
		{state: sandbox.StateSuspended, states: map[string]bool{"running": true}, want: false},
		{state: sandbox.StateReady, states: map[string]bool{"running": true}, want: true},
		{state: sandbox.StateReady, states: map[string]bool{"paused": true}, want: false},
	} {
		if got := matchesSandboxState(sandbox.Record{State: test.state}, test.states); got != test.want {
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
	store := newMemorySandboxStore()

	sandboxOps := &fakeSandboxOps{}
	runtimeService := newHandlerRuntime(sandboxOps, nil, store)
	runtimeService.SetSandboxDefaults(runtimeapi.SandboxDefaults{
		TemplateName: testTemplateNameDefault,
		VCPUNum:      4,
		VCPUMax:      4,
		RamMB:        256,
	})
	runtimeService.Templates = testTemplateStore()
	server := &Daemon{
		router:         http.NewServeMux(),
		sandboxStore:   store,
		runtimeService: runtimeService,
	}
	server.routes()

	if _, err := store.Create(context.Background(), sandbox.Record{
		ID:                       "sandbox-1",
		State:                    sandbox.StateReady,
		SourceTemplateName:       testTemplateNameExplicit,
		SourceTemplateID:         testTemplateIDExplicit,
		CheckpointHeadTemplateID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		VCPUNum:                  2,
		RamMB:                    128,
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
		if len(records) != 1 || records[0].SandboxID != "sandbox-1" || records[0].TemplateName != testTemplateNameExplicit || records[0].TemplateID != testTemplateIDExplicit {
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
		if record.SandboxID != "sandbox-1" || record.TemplateName != testTemplateNameExplicit || record.TemplateID != testTemplateIDExplicit || record.Domain == nil {
			t.Fatalf("get response = %#v", record)
		}
	})

	t.Run("create", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{
			"sandbox_id":"sandbox-2","template_name":"`+testTemplateNameOther+`","env":{"SOME_RANDOM_KEY":"key123"},
			"network":{"denyOut":["192.0.2.10"],"allowIn":["198.51.100.0/24"]}
		}`))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var record createSandboxResponse
		if err := json.NewDecoder(response.Body).Decode(&record); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
		if record.SandboxID != "sandbox-2" || record.TemplateName != testTemplateNameOther || record.TemplateID != testTemplateIDOther ||
			record.Domain != "192.0.2.2" || record.ConchInitAccessToken == "" {
			t.Fatalf("create response = %#v", record)
		}
		if got := sandboxOps.createReq.Env["SOME_RANDOM_KEY"]; got != "key123" {
			t.Fatalf("Env[SOME_RANDOM_KEY] = %q, want key123", got)
		}
		if sandboxOps.createReq.Network == nil || len(sandboxOps.createReq.Network.DenyOut) != 1 || len(sandboxOps.createReq.Network.AllowIn) != 1 {
			t.Fatalf("create network = %#v", sandboxOps.createReq.Network)
		}
	})

	t.Run("rejects invalid environment", func(t *testing.T) {
		for _, test := range []struct {
			body     string
			wantCode string
		}{
			{body: `{"sandbox_id":"invalid-env-key","template_name":"` + testTemplateNameOther + `","env":{"BAD=KEY":"value"}}`, wantCode: "sandbox.invalid_environment"},
			{body: `{"sandbox_id":"invalid-env-value","template_name":"` + testTemplateNameOther + `","env":{"KEY":123}}`, wantCode: "request.invalid_body"},
		} {
			createCalls := sandboxOps.createCalls
			response := serveSandboxRequest(server, http.MethodPost, "/api/v1/sandboxes", strings.NewReader(test.body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("body = %s, status = %d, response = %s", test.body, response.Code, response.Body.String())
			}
			var apiErr apiErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &apiErr); err != nil || string(apiErr.Code) != test.wantCode {
				t.Fatalf("body = %s, decoded response = %#v, error = %v", test.body, apiErr, err)
			}
			if test.wantCode != "request.invalid_body" {
				createCalls++
			}
			if sandboxOps.createCalls != createCalls {
				t.Fatalf("body = %s, runtime Create() calls = %d, want %d", test.body, sandboxOps.createCalls, createCalls)
			}
		}
	})

	t.Run("maps oversized initialization payload to bad request", func(t *testing.T) {
		sandboxOps.createErr = sandbox.ErrInitializationTooLarge.Wrap(agentprotocol.ErrPayloadTooLarge)
		t.Cleanup(func() { sandboxOps.createErr = nil })
		response := serveSandboxRequest(server, http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{
			"sandbox_id":"oversized-env","template_name":"`+testTemplateNameOther+`"
		}`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var apiErr apiErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &apiErr); err != nil || apiErr.Code != "sandbox.initialization_too_large" {
			t.Fatalf("decoded response = %#v, error = %v", apiErr, err)
		}
		sandboxOps.createErr = nil
	})

	t.Run("update network", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodPut, "/api/v1/sandboxes/sandbox-1/network", strings.NewReader(`{
			"allowOut":["192.0.2.20"],"denyIn":["198.51.100.20"]
		}`))
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if sandboxOps.updateReq.Network == nil || len(sandboxOps.updateReq.Network.AllowOut) != 1 || len(sandboxOps.updateReq.Network.DenyIn) != 1 {
			t.Fatalf("update network = %#v", sandboxOps.updateReq.Network)
		}
		getResponse := serveSandboxRequest(server, http.MethodGet, "/api/v1/sandboxes/sandbox-1", nil)
		var record sandboxInspectResponse
		if getResponse.Code != http.StatusOK || json.NewDecoder(getResponse.Body).Decode(&record) != nil {
			t.Fatalf("get after update status = %d, body = %s", getResponse.Code, getResponse.Body.String())
		}
		if record.Network == nil || len(record.Network.AllowOut) != 1 || len(record.Network.DenyIn) != 1 {
			t.Fatalf("get network = %#v", record.Network)
		}
	})

	t.Run("rejects invalid network", func(t *testing.T) {
		response := serveSandboxRequest(server, http.MethodPut, "/api/v1/sandboxes/sandbox-1/network", strings.NewReader(`{"allowOut":["example.com"]}`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		response = serveSandboxRequest(server, http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{
			"sandbox_id":"invalid-network","template_name":"`+testTemplateNameOther+`","network":{"denyIn":["example.com"]}
		}`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
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
		if len(sandboxOps.deleteReqs) != 1 || sandboxOps.deleteReqs[0] != "sandbox-1" {
			t.Fatalf("delete requests = %#v", sandboxOps.deleteReqs)
		}
		if _, err := store.Get(context.Background(), "sandbox-1"); !errors.Is(err, sandbox.ErrNotFound) {
			t.Fatalf("deleted sandbox lookup error = %v", err)
		}
	})
}

func serveSandboxRequest(server *Daemon, method, path string, body io.Reader) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.router.ServeHTTP(recorder, httptest.NewRequest(method, path, body))
	return recorder
}
