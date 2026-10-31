package e2bapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/pkg/requestid"
	"github.com/openeuler/Conch/pkg/ulog"
)

type failingRequestIDStore struct{ sandbox.Store }

func (s failingRequestIDStore) Get(context.Context, string) (sandbox.Record, error) {
	return sandbox.Record{}, errors.New("private store failure")
}

func (s failingRequestIDStore) List(context.Context, sandbox.Filter) ([]sandbox.Record, error) {
	return nil, errors.New("private store failure")
}

func TestRequestIDCorrelatesE2BErrors(t *testing.T) {
	previous := ulog.GetLogger()
	t.Cleanup(func() { ulog.SetLogger(previous) })
	logPath := filepath.Join(t.TempDir(), "e2b.log")
	if err := ulog.Init(ulog.Config{Level: ulog.InfoLevel, OutputFile: logPath}); err != nil {
		t.Fatal(err)
	}
	logger := ulog.GetLogger()
	t.Cleanup(func() {
		if closer, ok := logger.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})
	service, routes := testRuntime(t)
	service.Store = failingRequestIDStore{service.Store}
	api, err := New(Config{APIKey: testAPIKey, RequestTimeout: time.Second, MaxSandboxTTL: time.Hour}, service, routes)
	if err != nil {
		t.Fatal(err)
	}
	handler := requestid.Middleware(api)
	var ids []string
	for _, tc := range []struct{ path, id string }{
		{"/sandboxes", "e2b-list-request"},
		{"/sandboxes/unavailable", "e2b-get-request"},
		{"/sandboxes", ""},
		{"/sandboxes", "invalid id"},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("X-API-Key", testAPIKey)
		if tc.id != "" {
			r.Header.Set(requestid.Header, tc.id)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		id := w.Header().Get(requestid.Header)
		if w.Code != http.StatusInternalServerError || !requestid.Valid(id) {
			t.Fatalf("status = %d, request ID = %q", w.Code, id)
		}
		if requestid.Valid(tc.id) && id != tc.id {
			t.Fatalf("caller ID %q changed to %q", tc.id, id)
		}
		if strings.Contains(w.Body.String(), "private store failure") {
			t.Fatal("private error leaked to response")
		}
		ids = append(ids, id)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != len(ids) {
		t.Fatalf("expected one existing log per failure, got %s", data)
	}
	for i, line := range lines {
		if !strings.Contains(line, "[WARN]") || !strings.Contains(line, "E2B operation failed") ||
			!strings.Contains(line, "request_id="+ids[i]) || strings.Count(line, "request_id=") != 1 {
			t.Fatalf("uncorrelated failure log: %s", line)
		}
	}
}
