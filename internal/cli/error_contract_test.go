package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSandboxCreateMissingTemplateHasDeterministicExitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"version":1,"code":"not_found","resource_type":"template","message":"template not found","request_id":"req_test"}`))
	}))
	defer server.Close()
	t.Setenv("CONCH_API_URL", server.URL)

	if got := Run([]string{"sandbox", "create", "--template-id", "tmpl_missing"}); got != 3 {
		t.Fatalf("exit code = %d, want 3", got)
	}
}
