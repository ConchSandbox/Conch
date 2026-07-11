package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/netstack"
)

func TestHandleCreateSandboxInvalidNetworkPolicyReturnsBadRequest(t *testing.T) {
	sandboxOps := &fakeSandboxOps{createErr: netstack.ErrInvalidSandboxNetworkPolicy}
	server := newConvertHandlerServer(nil, nil, sandboxOps)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sandbox/create", bytes.NewBufferString(`{
		"sandbox_id":"sandbox-1",
		"namespace":"team-a",
		"network":{"allowOut":["example.com"]}
	}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Invalid network config") {
		t.Fatalf("create body = %q, want invalid network config error", rec.Body.String())
	}
}
