package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPrintSandboxHelpListsSandboxCommands(t *testing.T) {
	var buf bytes.Buffer
	printSandboxHelp(&buf)

	want := `Usage:
  conch sandbox <command> [options]

Commands:
  create      Create a sandbox from a Template ID.
  checkpoint  Checkpoint a sandbox into a resumable template.
  suspend     Suspend a running sandbox.
  resume      Resume a suspended sandbox.

Run 'conch sandbox <command> --help' for command-specific usage.
`
	if got := buf.String(); got != want {
		t.Fatalf("sandbox help output mismatch:\n%s", got)
	}
}

func TestRunSandboxCreateOmitsResourceOverridesByDefault(t *testing.T) {
	got := captureSandboxCreateRequest(t)
	for _, key := range []string{"vmm_name", "vcpu_num", "vcpu_max", "ram_mb"} {
		if value, ok := got[key]; ok {
			t.Fatalf("create request unexpectedly overrides daemon defaults with %s=%v; request = %#v", key, value, got)
		}
	}
}

func TestRunSandboxCreateKeepsExplicitRAM(t *testing.T) {
	got := captureSandboxCreateRequest(t, "--ram-mb", "4096")
	if got["ram_mb"] != float64(4096) {
		t.Fatalf("ram_mb = %v, want 4096; request = %#v", got["ram_mb"], got)
	}
	for _, key := range []string{"vmm_name", "vcpu_num", "vcpu_max"} {
		if value, ok := got[key]; ok {
			t.Fatalf("explicit RAM request unexpectedly overrides daemon default %s=%v; request = %#v", key, value, got)
		}
	}
}

func TestRunSandboxCreateRejectsNegativeRAM(t *testing.T) {
	var requestReceived atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestReceived.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]string{"sandboxID": "sandbox-123"})
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONCH_API_URL", server.URL)

	err := RunSandbox(context.Background(), []string{
		"create", "--template-id", "tmpl_123", "--sandbox-id", "sandbox-123", "--ram-mb", "-1",
	})
	if err == nil || !strings.Contains(err.Error(), "--ram-mb must not be negative") {
		t.Fatalf("RunSandbox() error = %v, want negative RAM validation", err)
	}
	if requestReceived.Load() {
		t.Fatal("negative RAM request reached conchd")
	}
}

func captureSandboxCreateRequest(t *testing.T, resourceArgs ...string) map[string]any {
	t.Helper()
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sandboxes" {
			t.Fatalf("path = %q, want /api/v1/sandboxes", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode create request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sandboxID": "sandbox-123"})
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONCH_API_URL", server.URL)

	args := []string{"create", "--template-id", "tmpl_123", "--sandbox-id", "sandbox-123"}
	args = append(args, resourceArgs...)
	if err := RunSandbox(context.Background(), args); err != nil {
		t.Fatalf("RunSandbox() error = %v", err)
	}
	return got
}
