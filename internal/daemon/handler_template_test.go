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

func TestTemplatePullAndPushHandlersUseRegistryBootIndex(t *testing.T) {
	const (
		reference = "registry.example.invalid/conch/template:latest"
		digest    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	imageOps := &fakeImageService{inspectReferenceResp: conchimage.BootIndexInfo{
		BootIndexDigest: digest,
		Resume:          true,
		VMMName:         "cloud-hypervisor",
	}}
	runtimeService := conchruntime.New(&fakeSandboxOps{}, imageOps, imageOps, store, "default")
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	pullRecorder := httptest.NewRecorder()
	pullRequest := httptest.NewRequest(http.MethodPost, "/api/template/pull", bytes.NewBufferString(`{
		"reference":"registry.example.invalid/conch/template:latest",
		"namespace":"team-a",
		"plain_http":true,
		"username":"pull-user",
		"password":"pull-pass",
		"labels":{"source":"registry"}
	}`))
	server.router.ServeHTTP(pullRecorder, pullRequest)
	if pullRecorder.Code != http.StatusOK {
		t.Fatalf("pull status = %d, body = %s", pullRecorder.Code, pullRecorder.Body.String())
	}
	var pullResponse struct {
		Status          string `json:"status"`
		TemplateID      string `json:"template_id"`
		BootIndexDigest string `json:"boot_index_digest"`
		BuildRef        string `json:"build_ref"`
	}
	if err := json.Unmarshal(pullRecorder.Body.Bytes(), &pullResponse); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	if pullResponse.Status != "ok" || pullResponse.TemplateID == "" || pullResponse.BootIndexDigest != digest || pullResponse.BuildRef != reference {
		t.Fatalf("pull response = %#v", pullResponse)
	}
	if imageOps.pullReq.ImageName != reference || imageOps.pullReq.Namespace != "team-a" || !imageOps.pullReq.SkipUnpack || !imageOps.pullReq.PlainHTTP {
		t.Fatalf("image pull request = %#v", imageOps.pullReq)
	}
	if imageOps.inspectReferenceReq != (bootIndexReferenceCall{Namespace: "team-a", Reference: reference}) {
		t.Fatalf("reference inspect request = %#v", imageOps.inspectReferenceReq)
	}
	rec, err := store.GetTemplate(context.Background(), pullResponse.TemplateID)
	if err != nil {
		t.Fatalf("GetTemplate() error = %v", err)
	}
	if rec.State != state.TemplateReady || rec.Origin != state.TemplateOriginCheckpoint || rec.BootIndexDigest != digest || rec.BuildRef != reference {
		t.Fatalf("pulled template = %#v", rec)
	}

	pushRecorder := httptest.NewRecorder()
	pushBody, _ := json.Marshal(templatePushRequest{
		TemplateID:      pullResponse.TemplateID,
		RemoteReference: "mirror.example.invalid/conch/template:copy",
		Namespace:       "team-a",
		PlainHTTP:       true,
		Username:        "push-user",
		Password:        "push-pass",
		RegistryTimeout: "10m",
	})
	pushRequest := httptest.NewRequest(http.MethodPost, "/api/template/push", bytes.NewReader(pushBody))
	server.router.ServeHTTP(pushRecorder, pushRequest)
	if pushRecorder.Code != http.StatusOK {
		t.Fatalf("push status = %d, body = %s", pushRecorder.Code, pushRecorder.Body.String())
	}
	if imageOps.pushBootIndexReq.BootIndexDigest != digest || imageOps.pushBootIndexReq.RemoteReference != "mirror.example.invalid/conch/template:copy" ||
		imageOps.pushBootIndexReq.Namespace != "team-a" || !imageOps.pushBootIndexReq.PlainHTTP || imageOps.pushBootIndexReq.RegistryTimeout != "10m" {
		t.Fatalf("Boot Index push request = %#v", imageOps.pushBootIndexReq)
	}
}
