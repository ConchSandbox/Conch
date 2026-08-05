package client

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openeuler/Conch/internal/apierror"
)

func TestCreateSandboxDecodesStableNotFoundError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"version":1,"code":"not_found","resource_type":"template","message":"template not found","request_id":"req_test"}`))
	}))
	defer server.Close()

	err := NewClient(server.URL).CreateSandbox("tmpl_missing", "sandbox-test", "default", 0)
	if !errors.Is(err, apierror.ErrTemplateNotFound) {
		t.Fatalf("error = %v, want template-not-found", err)
	}
	var got *apierror.Error
	if !errors.As(err, &got) || got.StatusCode != http.StatusNotFound || got.RequestID != "req_test" || got.ResourceType != apierror.ResourceTemplate {
		t.Fatalf("decoded error = %#v", got)
	}
}
