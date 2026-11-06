package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/pkg/requestid"
	"github.com/openeuler/Conch/pkg/ulog"
)

type correlatedSandboxOps struct {
	fakeSandboxOps
	id string
}

func (f *correlatedSandboxOps) Create(ctx context.Context, req sandbox.CreateRequest) (runtimeapi.SandboxCreateResult, error) {
	f.id = requestid.FromContext(ctx)
	return f.fakeSandboxOps.Create(ctx, req)
}

func TestDaemonRequestIDReachesRuntimeAndSandboxLogs(t *testing.T) {
	previous := ulog.GetLogger()
	t.Cleanup(func() { ulog.SetLogger(previous) })
	logPath := filepath.Join(t.TempDir(), "requests.log")
	if err := ulog.Init(ulog.Config{Level: ulog.InfoLevel, OutputFile: logPath}); err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		ops := &correlatedSandboxOps{}
		if fail {
			ops.createErr = errors.New("private failure detail")
		}
		svc := newHandlerRuntime(ops, nil, nil)
		svc.SetSandboxDefaults(conchruntime.SandboxDefaults{TemplateName: testTemplateNameDefault, VMMName: "stratovirt", VCPUNum: 2, VCPUMax: 2, RamMB: 1024})
		svc.Templates = testTemplateStore()
		d := &Daemon{router: http.NewServeMux(), runtimeService: svc}
		d.routes()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{"sandbox_id":"sandbox-a"}`))
		r.Header.Set(requestid.Header, "create-request-206")
		w := httptest.NewRecorder()
		newHTTPServer(d.router).Handler.ServeHTTP(w, r)
		wantStatus := http.StatusOK
		if fail {
			wantStatus = http.StatusInternalServerError
		}
		if w.Code != wantStatus || w.Header().Get(requestid.Header) != "create-request-206" || ops.id != "create-request-206" {
			t.Fatalf("status = %d, header ID = %q, sandbox ID = %q", w.Code, w.Header().Get(requestid.Header), ops.id)
		}
		if fail && strings.Contains(w.Body.String(), "private failure detail") {
			t.Fatal("private error leaked into response")
		}
	}
	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logs)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected only the existing success and failure logs, got %d: %s", len(lines), logs)
	}
	for _, marker := range []string{"Sandbox created successfully", "API request failed"} {
		found := false
		for _, line := range lines {
			if strings.Contains(line, marker) {
				found = true
				if !strings.Contains(line, "request_id=create-request-206") {
					t.Fatalf("uncorrelated log: %s", line)
				}
			}
		}
		if !found {
			t.Fatalf("missing log %q in %s", marker, logs)
		}
	}
}

func TestDaemonRequestIDOnAllHTTPResponses(t *testing.T) {
	d := &Daemon{router: http.NewServeMux()}
	d.routes()
	for _, tt := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/health", "", http.StatusServiceUnavailable},
		{http.MethodGet, "/missing", "", http.StatusNotFound},
		{http.MethodPatch, "/api/v1/sandboxes", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/sandboxes", "{", http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		newHTTPServer(d.router).Handler.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))
		if w.Code != tt.status || !requestid.Valid(w.Header().Get(requestid.Header)) {
			t.Fatalf("%s %s: status %d, ID %q", tt.method, tt.path, w.Code, w.Header().Get(requestid.Header))
		}
	}
}
