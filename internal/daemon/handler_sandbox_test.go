package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

const (
	testTemplateNameDefault  = "registry.example/conch/default:latest"
	testTemplateNameExplicit = "registry.example/conch/explicit:latest"
	testTemplateNameOther    = "registry.example/conch/other:latest"
	testTemplateIDDefault    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTemplateIDExplicit   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testTemplateIDOther      = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type templateStoreStub map[string]conchtemplate.Entry

func (s templateStoreStub) Put(_ context.Context, entry conchtemplate.Entry, _ ocispec.Descriptor) (conchtemplate.Entry, error) {
	s[entry.Name] = entry
	return entry, nil
}

func (s templateStoreStub) Get(_ context.Context, name string) (conchtemplate.Entry, error) {
	entry, ok := s[name]
	if !ok {
		return conchtemplate.Entry{}, conchtemplate.ErrNotFound.New()
	}
	return entry, nil
}

func (s templateStoreStub) List(context.Context, conchtemplate.Filter) ([]conchtemplate.Entry, error) {
	return nil, nil
}

func (s templateStoreStub) Delete(_ context.Context, name string) error {
	delete(s, name)
	return nil
}

func testTemplateStore() templateStoreStub {
	return templateStoreStub{
		testTemplateNameDefault: {
			Name: testTemplateNameDefault, Origin: conchtemplate.OriginImage, BootMode: conchtemplate.BootModeCold, BootIndexDigest: testTemplateIDDefault,
		},
		testTemplateNameExplicit: {
			Name: testTemplateNameExplicit, Origin: conchtemplate.OriginImage, BootMode: conchtemplate.BootModeCold, BootIndexDigest: testTemplateIDExplicit,
		},
		testTemplateNameOther: {
			Name: testTemplateNameOther, Origin: conchtemplate.OriginImage, BootMode: conchtemplate.BootModeCold, BootIndexDigest: testTemplateIDOther,
		},
	}
}

func TestHandleCreateSandboxReturnsGeneratedSandboxID(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	runtimeService := newHandlerRuntime(sandboxOps, nil, nil)
	runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{
		TemplateName: testTemplateNameDefault,
		VMMName:      "stratovirt",
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
	})
	runtimeService.Templates = testTemplateStore()
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(`{}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response createSandboxResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.SandboxID == "" || response.SandboxID != sandboxOps.createReq.SandboxID {
		t.Fatalf("sandbox identity = response:%q request:%q", response.SandboxID, sandboxOps.createReq.SandboxID)
	}
	if sandboxOps.createReq.TemplateID != testTemplateIDDefault {
		t.Fatalf("Boot Index digest = %q, want daemon default", sandboxOps.createReq.TemplateID)
	}
}

func TestRemoveAllSandboxesDeletesRuntimeAndStateRecords(t *testing.T) {
	store := newMemorySandboxStore()

	records := []sandbox.Record{
		{
			ID:                       "sandbox-ready",
			State:                    sandbox.StateReady,
			CheckpointHeadTemplateID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		{
			ID:               "sandbox-creating",
			State:            sandbox.StateCreating,
			SourceTemplateID: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}
	for _, record := range records {
		if _, err := store.Create(context.Background(), record); err != nil {
			t.Fatalf("seed sandbox %s: %v", record.ID, err)
		}
	}

	sandboxOps := &fakeSandboxOps{}
	server := &Daemon{
		sandboxStore:   store,
		runtimeService: newHandlerRuntime(sandboxOps, nil, store),
	}

	if err := server.removeAllSandboxes(); err != nil {
		t.Fatalf("removeAllSandboxes() error = %v", err)
	}
	if len(sandboxOps.deleteReqs) != len(records) {
		t.Fatalf("delete requests = %d, want %d", len(sandboxOps.deleteReqs), len(records))
	}
	remaining, err := store.List(context.Background(), sandbox.Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining sandbox records = %#v, want empty", remaining)
	}
}

func TestHandleCreateSandboxTemplateSelection(t *testing.T) {
	tests := []struct {
		name                string
		defaultTemplateName string
		body                string
		wantStatus          int
		wantTemplateID      string
	}{
		{name: "omitted uses configured default", defaultTemplateName: testTemplateNameDefault, body: `{}`, wantStatus: http.StatusOK, wantTemplateID: testTemplateIDDefault},
		{name: "whitespace default is rejected", defaultTemplateName: " \t ", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "absent default is rejected", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "explicit template wins", defaultTemplateName: testTemplateNameDefault, body: `{"template_name":"` + testTemplateNameExplicit + `"}`, wantStatus: http.StatusOK, wantTemplateID: testTemplateIDExplicit},
		{name: "name and ID are rejected", body: `{"template_name":"` + testTemplateNameExplicit + `","template_id":"` + testTemplateIDExplicit + `"}`, wantStatus: http.StatusBadRequest},
		{name: "invalid ID is rejected", defaultTemplateName: testTemplateNameDefault, body: `{"template_id":"not-a-digest"}`, wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxOps := &fakeSandboxOps{}
			runtimeService := newHandlerRuntime(sandboxOps, nil, nil)
			runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{
				TemplateName: tt.defaultTemplateName,
				VCPUNum:      2,
				VCPUMax:      2,
				RamMB:        1024,
			})
			runtimeService.Templates = testTemplateStore()
			server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
			server.routes()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(tt.body))
			server.router.ServeHTTP(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantStatus == http.StatusBadRequest {
				var response map[string]string
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if response["status"] != "error" || response["error"] != sandbox.ErrInvalidArgument.Error() {
					t.Fatalf("error response = %#v", response)
				}
				return
			}
			if sandboxOps.createReq.TemplateID != tt.wantTemplateID {
				t.Fatalf("Boot Index digest = %q, want %q", sandboxOps.createReq.TemplateID, tt.wantTemplateID)
			}
		})
	}
}

func TestHandleCreateSandboxRejectsMissingResources(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	runtimeService := newHandlerRuntime(sandboxOps, nil, nil)
	runtimeService.Templates = testTemplateStore()
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(`{"template_name":"`+testTemplateNameExplicit+`","sandbox_id":"sandbox-123"}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestHandleCreateSandboxRejectsRAMBelowMinimum(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	runtimeService := newHandlerRuntime(sandboxOps, nil, nil)
	runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{TemplateName: testTemplateNameDefault})
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(`{"ram_mb":64}`))
	server.router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if sandboxOps.createReq.SandboxID != "" {
		t.Fatalf("runtime Create() was called: %#v", sandboxOps.createReq)
	}
}

func TestHandleCreateSandboxReturnsConflictForExistingID(t *testing.T) {
	store := newMemorySandboxStore()
	if _, err := store.Create(context.Background(), sandbox.Record{
		ID:                       "sandbox-1",
		State:                    sandbox.StateReady,
		CheckpointHeadTemplateID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatalf("Create() seed error = %v", err)
	}

	runtimeService := newHandlerRuntime(&fakeSandboxOps{}, nil, store)
	runtimeService.Templates = testTemplateStore()
	runtimeService.SetSandboxDefaults(runtimeapi.SandboxDefaults{VCPUNum: 1, VCPUMax: 1, RamMB: 128})
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(`{"sandbox_id":"sandbox-1","template_name":"`+testTemplateNameOther+`"}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

func TestHandleInspectMissingTemplateReturnsDomainError(t *testing.T) {
	store := newMemorySandboxStore()

	runtimeService := newHandlerRuntime(nil, nil, store)
	runtimeService.Templates = missingTemplateStore{}
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/template/inspect",
		bytes.NewBufferString(`{"name":"registry.example/conch/missing:latest"}`),
	)
	server.router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response apiErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Code != "template.not_found" {
		t.Fatalf("response = %#v, error = %v", response, err)
	}
}

type missingTemplateStore struct{}

func (missingTemplateStore) Put(context.Context, conchtemplate.Entry, ocispec.Descriptor) (conchtemplate.Entry, error) {
	return conchtemplate.Entry{}, conchtemplate.ErrNotFound.New()
}

func (missingTemplateStore) Get(context.Context, string) (conchtemplate.Entry, error) {
	return conchtemplate.Entry{}, conchtemplate.ErrNotFound.New()
}

func (missingTemplateStore) List(context.Context, conchtemplate.Filter) ([]conchtemplate.Entry, error) {
	return nil, nil
}

func (missingTemplateStore) Delete(context.Context, string) error { return nil }

type balloonSandboxOps struct {
	fakeSandboxOps
	calls   int
	id      string
	target  *int64
	timeout time.Duration
	err     error
}

func (f *balloonSandboxOps) Balloon(_ context.Context, id string, target *int64, timeout time.Duration) (runtimeapi.BalloonInfo, error) {
	f.calls++
	f.id, f.target, f.timeout = id, target, timeout
	return runtimeapi.BalloonInfo{ConfiguredMB: 4096, TargetMB: target, ActualMB: 4096}, f.err
}

func TestHandleSandboxBalloon(t *testing.T) {
	for _, tt := range []struct {
		name, method, body, code string
		err                      error
		status                   int
		wantCall                 bool
	}{
		{name: "query", method: http.MethodGet, status: 200, wantCall: true},
		{name: "set target", method: http.MethodPut, body: `{"target_mb":1024,"timeout_seconds":3}`, status: 200, wantCall: true},
		{name: "default timeout", method: http.MethodPut, body: `{"target_mb":1024}`, status: 200, wantCall: true},
		{name: "missing target", method: http.MethodPut, body: `{}`, status: 400, code: "sandbox.invalid_argument"},
		{name: "zero timeout", method: http.MethodPut, body: `{"target_mb":1024,"timeout_seconds":0}`, status: 400, code: "sandbox.invalid_argument"},
		{name: "timeout too long", method: http.MethodPut, body: `{"target_mb":1024,"timeout_seconds":61}`, status: 400, code: "sandbox.invalid_argument"},
		{name: "malformed body", method: http.MethodPut, body: "{", status: 400, code: "request.invalid_body"},
		{name: "not found", method: http.MethodGet, err: sandbox.ErrNotFound.New(), status: 404, code: "sandbox.not_found", wantCall: true},
		{name: "runtime dead", method: http.MethodGet, err: sandbox.ErrRuntimeDead.New(), status: 409, code: "sandbox.runtime_dead", wantCall: true},
		{name: "VMM failure", method: http.MethodGet, err: errors.New("private QMP error"), status: 500, code: "internal", wantCall: true},
		{name: "deadline", method: http.MethodGet, err: context.DeadlineExceeded, status: 504, code: "request.deadline_exceeded", wantCall: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ops := &balloonSandboxOps{err: tt.err}
			server := &Daemon{router: http.NewServeMux(), runtimeService: newHandlerRuntime(ops, nil, nil)}
			server.routes()
			recorder := httptest.NewRecorder()
			server.router.ServeHTTP(recorder, httptest.NewRequest(tt.method, "/api/v1/sandboxes/sandbox-a/balloon", bytes.NewBufferString(tt.body)))
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if (ops.calls == 1) != tt.wantCall {
				t.Fatalf("balloon calls = %d, wantCall = %v", ops.calls, tt.wantCall)
			}
			if tt.status != 200 {
				var response apiErrorResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if string(response.Code) != tt.code {
					t.Fatalf("error response = %+v", response)
				}
				if tt.status == 500 && response.Error != "internal server error" {
					t.Fatalf("private VMM error exposed: %s", response.Error)
				}
				if tt.status == 504 && response.Error != "balloon operation timed out while submitting the target or querying memory; the target may remain active" {
					t.Fatalf("unexpected timeout message: %s", response.Error)
				}
				return
			}
			wantTimeout := 20 * time.Second
			if tt.name == "set target" {
				wantTimeout = 3 * time.Second
			}
			if ops.id != "sandbox-a" || ops.timeout != wantTimeout {
				t.Fatalf("forwarded id=%q timeout=%v", ops.id, ops.timeout)
			}
			var info runtimeapi.BalloonInfo
			if err := json.Unmarshal(recorder.Body.Bytes(), &info); err != nil {
				t.Fatal(err)
			}
			if info.ConfiguredMB != 4096 || info.ActualMB != 4096 {
				t.Fatalf("response = %+v", info)
			}
			if tt.method == http.MethodGet {
				if ops.target != nil || info.TargetMB != nil {
					t.Fatal("GET unexpectedly submitted a target")
				}
			} else if ops.target == nil || *ops.target != 1024 || info.TargetMB == nil || *info.TargetMB != 1024 {
				t.Fatalf("PUT target not preserved: %+v", info)
			}
		})
	}
}
