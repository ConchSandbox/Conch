package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
)

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
	if err := store.UpsertTemplate(ctx, state.TemplateRecord{
		ID:              "tmpl-source",
		Origin:          state.TemplateOriginImage,
		Namespace:       "default",
		State:           state.TemplateReady,
		BootIndexDigest: sourceDigest,
		BootMode:        state.TemplateBootModeCold,
	}); err != nil {
		t.Fatalf("UpsertTemplate() error = %v", err)
	}
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:          "pod-1",
		ConchSandboxID:        "sandbox-1",
		Namespace:             "default",
		State:                 state.SandboxReady,
		SourceTemplateID:      "tmpl-source",
		SourceBootIndexDigest: sourceDigest,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	sandboxOps := &fakeSandboxOps{}
	imageOps := &fakeImageService{checkpointPublishResp: conchimage.PublishCheckpointBootImageResult{
		BootIndexDigest: checkpointDigest,
		ImageName:       "localhost/conch/template:checkpoint",
	}}
	runtimeService := conchruntime.New(sandboxOps, imageOps, imageOps, store, "default")
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/sandbox/checkpoint", bytes.NewBufferString(`{"sandbox_id":"pod-1"}`))
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
	if sandboxOps.checkpointReq.SandboxId != "sandbox-1" {
		t.Fatalf("checkpoint request = %#v", sandboxOps.checkpointReq)
	}
	if imageOps.checkpointPublishReq.SourceBootIndexDigest != sourceDigest {
		t.Fatalf("publish request = %#v", imageOps.checkpointPublishReq)
	}
}
