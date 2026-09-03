package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/openeuler/Conch/internal/conchruntime"
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
	runtimeService := conchruntime.New(sandboxOps, nil, nil)
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
		runtimeService: conchruntime.New(sandboxOps, nil, store),
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
			runtimeService := conchruntime.New(sandboxOps, nil, nil)
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
	runtimeService := conchruntime.New(sandboxOps, nil, nil)
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
	runtimeService := conchruntime.New(sandboxOps, nil, nil)
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

	runtimeService := conchruntime.New(&fakeSandboxOps{}, nil, store)
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

	runtimeService := conchruntime.New(nil, nil, store)
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
