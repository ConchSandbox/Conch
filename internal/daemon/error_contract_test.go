package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/apierror"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

func TestSandboxCreateMissingTemplateUsesVersionedNotFoundEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name            string
		body            string
		defaultTemplate string
	}{
		{name: "explicit template", body: `{"template_id":"tmpl_missing"}`},
		{name: "daemon default", body: `{}`, defaultTemplate: "tmpl_missing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runtimeService := conchruntime.New(&fakeSandboxOps{createErr: conchtemplate.ErrNotFound}, nil, nil, nil)
			runtimeService.SetSandboxDefaults(conchruntime.SandboxDefaults{TemplateID: tt.defaultTemplate})
			server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
			server.routes()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", strings.NewReader(tt.body))
			request.Header.Set("X-Request-ID", "req_test")
			server.router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var got apierror.Envelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			want := apierror.Envelope{Version: 1, Code: apierror.CodeNotFound, ResourceType: apierror.ResourceTemplate, Message: "template not found", RequestID: "req_test"}
			if got != want || recorder.Header().Get("X-Request-ID") != got.RequestID {
				t.Fatalf("response = %#v, headers = %#v, want %#v", got, recorder.Header(), want)
			}
		})
	}
}

func TestSandboxCreateInternalErrorsDoNotLeakDetails(t *testing.T) {
	runtimeService := conchruntime.New(&fakeSandboxOps{createErr: errors.New("credential=super-secret")}, nil, nil, nil)
	server := &Daemon{router: http.NewServeMux(), runtimeService: runtimeService}
	server.routes()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{"template_id":"tmpl_present"}`))
	server.router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError || bytes.Contains(recorder.Body.Bytes(), []byte("super-secret")) {
		t.Fatalf("unexpected response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var got apierror.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil || got.Code != apierror.CodeInternalError || got.Message != "internal server error" {
		t.Fatalf("internal error envelope = %#v, decode error = %v", got, err)
	}
}

func TestMissingTemplateLifecycleEndpointsUseNotFoundEnvelope(t *testing.T) {
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := &Daemon{router: http.NewServeMux(), runtimeService: conchruntime.New(nil, nil, nil, store)}
	server.routes()

	for _, path := range []string{"/api/template/inspect", "/api/template/remove"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"id":"tmpl_missing"}`))
			server.router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var got apierror.Envelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil || got.Code != apierror.CodeNotFound || got.ResourceType != apierror.ResourceTemplate {
				t.Fatalf("envelope = %#v, decode error = %v", got, err)
			}
		})
	}
}
