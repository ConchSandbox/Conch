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

	"github.com/openeuler/Conch/internal/cli/client"
)

func TestPrintSandboxHelpListsSandboxCommands(t *testing.T) {
	var buf bytes.Buffer
	printSandboxHelp(&buf)

	want := `Usage:
  conch sandbox <command> [options]

Commands:
  create      Create a sandbox from a Template ID or the daemon default.
  checkpoint  Checkpoint a sandbox into a resumable template.
  suspend     Suspend a running sandbox.
  resume      Resume a suspended sandbox.
  delete      Delete a sandbox.
  ls          List sandboxes.

Run 'conch sandbox <command> --help' for command-specific usage.
`
	if got := buf.String(); got != want {
		t.Fatalf("sandbox help output mismatch:\n%s", got)
	}
}

func TestRunSandboxCreateOmitsResourceOverridesByDefault(t *testing.T) {
	got := captureSandboxCreateRequest(t)
	for _, key := range []string{"template_id", "vmm_name", "vcpu_num", "vcpu_max", "ram_mb"} {
		if value, ok := got[key]; ok {
			t.Fatalf("create request unexpectedly overrides daemon defaults with %s=%v; request = %#v", key, value, got)
		}
	}
}

func TestRunSandboxCreateKeepsExplicitTemplateID(t *testing.T) {
	got := captureSandboxCreateRequest(t, "--template-id", "sha256:explicit")
	if got["template_id"] != "sha256:explicit" {
		t.Fatalf("template_id = %v; request = %#v", got["template_id"], got)
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
		"create", "--template-id", "sha256:test", "--sandbox-id", "sandbox-123", "--ram-mb", "-1",
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

	args := []string{"create", "--sandbox-id", "sandbox-123"}
	args = append(args, resourceArgs...)
	if err := RunSandbox(context.Background(), args); err != nil {
		t.Fatalf("RunSandbox() error = %v", err)
	}
	return got
}

func TestPrintSandboxList(t *testing.T) {
	var out bytes.Buffer
	records := []client.SandboxRecord{
		{SandboxID: "sandbox-b", TemplateID: "sha256:b", CPUCount: 2, MemoryMB: 512, StartedAt: "2026-01-02T03:04:05Z"},
		{SandboxID: "sandbox-a", TemplateID: "sha256:a", CPUCount: 1, MemoryMB: 256, StartedAt: "2026-01-01T03:04:05Z"},
	}
	if err := printSandboxList(&out, records); err != nil {
		t.Fatalf("printSandboxList() error = %v", err)
	}
	want := "ID         TEMPLATE_ID  CPU  MEMORY_MB  STARTED_AT\n" +
		"sandbox-a  sha256:a     1    256        2026-01-01T03:04:05Z\n" +
		"sandbox-b  sha256:b     2    512        2026-01-02T03:04:05Z\n"
	if got := out.String(); got != want {
		t.Fatalf("sandbox list output = %q, want %q", got, want)
	}
}
