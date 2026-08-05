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
	conchimage "github.com/openeuler/Conch/internal/image"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

func TestHandleCreateSandboxReturnsGeneratedSandboxID(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	runtimeService := conchruntime.New(sandboxOps, nil, nil, nil)
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
			runtimeService := conchruntime.New(sandboxOps, nil, nil, nil)
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
	runtimeService := conchruntime.New(sandboxOps, nil, nil, nil)
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

	runtimeService := conchruntime.New(&fakeSandboxOps{}, nil, nil, store)
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewBufferString(`{"sandbox_id":"sandbox-1","template_id":"tmpl-new"}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

func TestHandleCheckpointSandboxReturnsBootIndexDigest(t *testing.T) {
	const (
		sourceDigest     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		checkpointDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)

	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	if err := store.CreateTemplate(ctx, conchtemplate.Entry{
		ID:              "tmpl-source",
		Origin:          conchtemplate.OriginImage,
		BootIndexDigest: sourceDigest,
		BootMode:        conchtemplate.BootModeCold,
	}); err != nil {
		t.Fatalf("CreateTemplate() error = %v", err)
	}
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID:                     "sandbox-1",
		CheckpointHeadTemplateID:      "tmpl-source",
		CheckpointHeadBootIndexDigest: sourceDigest,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	sandboxOps := &fakeSandboxOps{}
	imageOps := &fakeImageService{checkpointPublishResp: conchimage.PublishCheckpointBootImageResult{
		BootIndexDigest: checkpointDigest,
		ImageName:       "localhost/conch/template:checkpoint",
	}}
	runtimeService := conchruntime.New(sandboxOps, imageOps, imageOps, store)
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/sandbox/checkpoint", bytes.NewBufferString(`{"sandbox_id":"sandbox-1"}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		Status          string `json:"status"`
		TemplateID      string `json:"template_id"`
		BootIndexDigest string `json:"boot_index_digest"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Status != "ok" || response.TemplateID == "" || response.BootIndexDigest != checkpointDigest {
		t.Fatalf("checkpoint response = %#v", response)
	}
	if sandboxOps.checkpointReq.SandboxID != "sandbox-1" {
		t.Fatalf("checkpoint request = %#v", sandboxOps.checkpointReq)
	}
	if imageOps.checkpointPublishReq.SourceBootIndexDigest != sourceDigest {
		t.Fatalf("publish request = %#v", imageOps.checkpointPublishReq)
	}
}
