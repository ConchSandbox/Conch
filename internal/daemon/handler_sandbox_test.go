package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

const (
	testTemplateIDDefault  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTemplateIDExplicit = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testTemplateIDOther    = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestHandleCreateSandboxReturnsGeneratedSandboxID(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	runtimeService := conchruntime.New(sandboxOps, nil, nil)
	runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{
		TemplateID: testTemplateIDDefault,
		VMMName:    "stratovirt",
		VCPUNum:    2,
		VCPUMax:    2,
		RamMB:      1024,
	})
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
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ids := []string{"sandbox-a", "sandbox-b"}
	for _, id := range ids {
		if err := store.UpsertSandbox(context.Background(), state.SandboxRecord{
			SandboxID:                id,
			CheckpointHeadTemplateID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}); err != nil {
			t.Fatalf("seed sandbox %s: %v", id, err)
		}
	}

	sandboxOps := &fakeSandboxOps{}
	server := &Daemon{
		stateStore:     store,
		runtimeService: conchruntime.New(sandboxOps, nil, store),
	}

	if err := server.removeAllSandboxes(); err != nil {
		t.Fatalf("removeAllSandboxes() error = %v", err)
	}
	if len(sandboxOps.deleteReqs) != len(ids) {
		t.Fatalf("delete requests = %d, want %d", len(sandboxOps.deleteReqs), len(ids))
	}
	remaining, err := store.ListSandboxes(context.Background())
	if err != nil {
		t.Fatalf("ListSandboxes() error = %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining sandbox records = %#v, want empty", remaining)
	}
}

func TestHandleCreateSandboxTemplateSelection(t *testing.T) {
	tests := []struct {
		name            string
		defaultTemplate string
		body            string
		wantStatus      int
		wantTemplate    string
	}{
		{name: "omitted uses configured default", defaultTemplate: testTemplateIDDefault, body: `{}`, wantStatus: http.StatusOK, wantTemplate: testTemplateIDDefault},
		{name: "whitespace default is rejected", defaultTemplate: " \t ", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "absent default is rejected", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "explicit template wins", defaultTemplate: testTemplateIDDefault, body: `{"template_id":"` + testTemplateIDExplicit + `"}`, wantStatus: http.StatusOK, wantTemplate: testTemplateIDExplicit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxOps := &fakeSandboxOps{}
			runtimeService := conchruntime.New(sandboxOps, nil, nil)
			runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{
				TemplateID: tt.defaultTemplate,
				VCPUNum:    2,
				VCPUMax:    2,
				RamMB:      1024,
			})
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
			if sandboxOps.createReq.TemplateID != tt.wantTemplate {
				t.Fatalf("Boot Index digest = %q, want %q", sandboxOps.createReq.TemplateID, tt.wantTemplate)
			}
		})
	}
}

func TestHandleCreateSandboxRejectsMissingResources(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	runtimeService := conchruntime.New(sandboxOps, nil, nil)
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(`{"template_id":"`+testTemplateIDExplicit+`","sandbox_id":"sandbox-123"}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestHandleCreateSandboxRejectsRAMBelowMinimum(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	runtimeService := conchruntime.New(sandboxOps, nil, nil)
	runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{TemplateID: testTemplateIDDefault})
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
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertSandbox(context.Background(), state.SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatalf("UpsertSandbox() seed error = %v", err)
	}

	runtimeService := conchruntime.New(&fakeSandboxOps{}, nil, store)
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(`{"sandbox_id":"sandbox-1","template_id":"`+testTemplateIDOther+`"}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

func TestHandleInspectMissingTemplateReturnsDomainError(t *testing.T) {
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	runtimeService := conchruntime.New(nil, nil, store)
	runtimeService.Templates = missingTemplateStore{}
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()
	recorder := httptest.NewRecorder()
	missingDigest := digest.FromString("missing-template").String()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/template/inspect",
		bytes.NewBufferString(`{"template_id":"`+missingDigest+`"}`),
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

func (missingTemplateStore) Create(context.Context, conchtemplate.Entry, ocispec.Descriptor, ...conchtemplate.CreateOptions) (conchtemplate.Entry, error) {
	return conchtemplate.Entry{}, conchtemplate.ErrNotFound.New()
}

func (missingTemplateStore) Get(context.Context, string) (conchtemplate.Entry, error) {
	return conchtemplate.Entry{}, conchtemplate.ErrNotFound.New()
}

func (missingTemplateStore) List(context.Context, conchtemplate.Filter) ([]conchtemplate.Entry, error) {
	return nil, nil
}

func (missingTemplateStore) Delete(context.Context, string) error { return nil }
