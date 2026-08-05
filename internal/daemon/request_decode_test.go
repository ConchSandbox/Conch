package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/apierror"
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
			request.Header.Set("X-Request-ID", "req_unknown_field")
			server.router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var got apierror.Envelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			if got.Code != apierror.CodeInvalidArgument || got.RequestID != "req_unknown_field" ||
				!strings.Contains(got.Message, tt.unknownField) {
				t.Fatalf("envelope = %#v", got)
			}
			if recorder.Header().Get("X-Request-ID") != got.RequestID {
				t.Fatalf("X-Request-ID = %q, body request_id = %q", recorder.Header().Get("X-Request-ID"), got.RequestID)
			}
			if sandboxOps.createReq.SandboxID != "" {
				t.Fatalf("sandbox create was called: %#v", sandboxOps.createReq)
			}
		})
	}
}
