package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/daemon/state"
)

func TestHandleCreateSandboxReturnsGeneratedSandboxID(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	runtimeService := conchruntime.New(sandboxOps, nil, nil)
	runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{TemplateID: "tmpl-default"})
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
	if sandboxOps.createReq.TemplateID != "tmpl-default" {
		t.Fatalf("template ID = %q, want daemon default", sandboxOps.createReq.TemplateID)
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
		{name: "omitted uses configured default", defaultTemplate: "tmpl-default", body: `{}`, wantStatus: http.StatusOK, wantTemplate: "tmpl-default"},
		{name: "whitespace default is rejected", defaultTemplate: " \t ", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "absent default is rejected", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "explicit template wins", defaultTemplate: "tmpl-default", body: `{"template_id":"tmpl-explicit"}`, wantStatus: http.StatusOK, wantTemplate: "tmpl-explicit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxOps := &fakeSandboxOps{}
			runtimeService := conchruntime.New(sandboxOps, nil, nil)
			runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{TemplateID: tt.defaultTemplate})
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
				if response["status"] != "error" || response["error"] != conchruntime.ErrTemplateIDRequired.Error() {
					t.Fatalf("error response = %#v", response)
				}
				return
			}
			if sandboxOps.createReq.TemplateID != tt.wantTemplate {
				t.Fatalf("template ID = %q, want %q", sandboxOps.createReq.TemplateID, tt.wantTemplate)
			}
		})
	}
}

func TestHandleCreateSandboxUsesConfiguredDefaultsForOmittedResources(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	runtimeService := conchruntime.New(sandboxOps, nil, nil)
	defaults := config.DefaultConfig().Sandbox
	runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{
		VMMName: defaults.DefaultVMMName,
		VCPUNum: defaults.DefaultVCPUNum,
		VCPUMax: defaults.DefaultVCPUMax,
		RamMB:   defaults.DefaultRAMMB,
	})
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(`{"template_id":"tmpl_123","sandbox_id":"sandbox-123"}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	got := sandboxOps.createReq
	if got.VMMName != defaults.DefaultVMMName || got.VCPUNum != defaults.DefaultVCPUNum ||
		got.VCPUMax != defaults.DefaultVCPUMax || got.RAMMB != defaults.DefaultRAMMB {
		t.Fatalf("resources = vmm:%q vcpu:%d/%d ram:%d; want vmm:%q vcpu:%d/%d ram:%d",
			got.VMMName, got.VCPUNum, got.VCPUMax, got.RAMMB,
			defaults.DefaultVMMName, defaults.DefaultVCPUNum, defaults.DefaultVCPUMax, defaults.DefaultRAMMB)
	}
}

func TestHandleCreateSandboxReturnsConflictForExistingID(t *testing.T) {
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertSandbox(context.Background(), state.SandboxRecord{
		SandboxID:                     "sandbox-1",
		CheckpointHeadTemplateID:      "tmpl-existing",
		CheckpointHeadBootIndexDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatalf("UpsertSandbox() seed error = %v", err)
	}

	runtimeService := conchruntime.New(&fakeSandboxOps{}, nil, store)
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(`{"sandbox_id":"sandbox-1","template_id":"tmpl-new"}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}
