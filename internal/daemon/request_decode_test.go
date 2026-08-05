package daemon

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/conchruntime"
)

func TestDecodeStrictJSON(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "known fields", body: `{"template_id":"tmpl_123","volumeMounts":[{"source":"/tmp/data","path":"/data","readonly":true}]}`},
		{name: "trailing whitespace", body: "{\"template_id\":\"tmpl_123\"}\n\t"},
		{name: "unknown top-level field", body: `{"template_id":"tmpl_123","volume_mounts":[]}`, wantErr: true},
		{name: "unknown nested field", body: `{"template_id":"tmpl_123","volumeMounts":[{"source":"/tmp/data","path":"/data","read_only":true}]}`, wantErr: true},
		{name: "multiple values", body: `{"template_id":"tmpl_123"}{"sandbox_id":"sandbox-2"}`, wantErr: true},
		{name: "trailing garbage", body: `{"template_id":"tmpl_123"} trailing`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req sandboxCreateRequest
			err := decodeStrictJSON(strings.NewReader(tt.body), &req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeStrictJSON() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestJSONHandlersRejectUnknownFields(t *testing.T) {
	imageOps := &fakeImageService{}
	snapshotOps := &fakeSnapshotService{}
	sandboxOps := &fakeSandboxOps{}
	runtimeService := conchruntime.New(sandboxOps, imageOps, imageOps, nil, "default")
	runtimeService.Snapshot = snapshotOps
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()

	paths := []string{
		"/api/v1/sandboxes",
		"/api/sandbox/suspend",
		"/api/sandbox/resume",
		"/api/sandbox/checkpoint",
		"/api/template/pull",
		"/api/template/push",
		"/api/template/list",
		"/api/template/inspect",
		"/api/template/remove",
		"/api/image/pull",
		"/api/image/push",
		"/api/image/list",
		"/api/image/remove",
		"/api/image/unpack",
		"/api/snapshot/info",
		"/api/snapshot/list",
		"/api/snapshot/remove",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"unexpected":true}`))
			server.router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), `unknown field "unexpected"`) {
				t.Fatalf("body = %q", recorder.Body.String())
			}
		})
	}

	if sandboxOps.createReq.SandboxID != "" || sandboxOps.suspendReq.SandboxID != "" ||
		sandboxOps.resumeReq.SandboxID != "" || sandboxOps.checkpointReq.SandboxID != "" {
		t.Fatalf("sandbox backend was called: %#v", sandboxOps)
	}
	if imageOps.pullReq.ImageName != "" || imageOps.pushReq.LocalImage != "" ||
		imageOps.removeReq.ImageName != "" || imageOps.unpackReq.ImageName != "" || imageOps.listReq.Namespace != "" {
		t.Fatalf("image backend was called: %#v", imageOps)
	}
	if snapshotOps.infoReq.Key != "" || snapshotOps.removeReq.Key != "" || snapshotOps.listReq.Namespace != "" {
		t.Fatalf("snapshot backend was called: %#v", snapshotOps)
	}
}

func TestTemplateCreateRejectsUnknownMetadataField(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("metadata", `{"source":"example.invalid/image:latest","unexpected":true}`); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart body: %v", err)
	}

	server := &Daemon{router: http.NewServeMux(), runtimeService: conchruntime.New(nil, nil, nil, nil)}
	server.routes()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/template/create", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	server.router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `unknown field "unexpected"`) {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

func TestTemplateCreateAcceptsAllMetadataFields(t *testing.T) {
	metadata := `{
		"source":"example.invalid/image:latest",
		"namespace":"team-a",
		"boot_index_tag":"example.invalid/conch/boot:latest",
		"plain_http":true,
		"username":"tester",
		"password":"secret",
		"labels":{"purpose":"strict-json-test"}
	}`

	var req templateCreateRequest
	if err := decodeStrictJSON(strings.NewReader(metadata), &req); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if req.Source != "example.invalid/image:latest" || req.Namespace != "team-a" ||
		req.BootIndexTag != "example.invalid/conch/boot:latest" || !req.PlainHTTP ||
		req.Username != "tester" || req.Password != "secret" ||
		req.Labels["purpose"] != "strict-json-test" {
		t.Fatalf("decoded metadata = %#v", req)
	}
}

func TestSandboxCreateRejectsUnknownFieldsWithoutSideEffects(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		unknownField string
	}{
		{
			name:         "top-level field",
			body:         `{"template_id":"tmpl_123","sandbox_id":"must-not-exist","volume_mounts":[]}`,
			unknownField: "volume_mounts",
		},
		{
			name:         "nested field",
			body:         `{"template_id":"tmpl_123","sandbox_id":"must-not-exist","volumeMounts":[{"source":"/tmp/data","path":"/data","read_only":true}]}`,
			unknownField: "read_only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxOps := &fakeSandboxOps{}
			runtimeService := conchruntime.New(sandboxOps, nil, nil, nil, "default")
			server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
			server.routes()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", strings.NewReader(tt.body))
			server.router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tt.unknownField) {
				t.Fatalf("body = %q, want unknown field %q", recorder.Body.String(), tt.unknownField)
			}
			if sandboxOps.createReq.SandboxID != "" {
				t.Fatalf("sandbox create was called: %#v", sandboxOps.createReq)
			}
		})
	}
}
