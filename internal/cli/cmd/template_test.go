package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/image/client"
)

func TestRunTemplatePullUsesProgressStream(t *testing.T) {
	var request client.TemplatePullRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/template/pull/stream" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"status":"started"}` + "\n"))
		_, _ = w.Write([]byte(`{"status":"downloading","component":"rootfs","progress":100,"total":100}` + "\n"))
		_, _ = w.Write([]byte(`{"status":"completed","template":{"status":"ok","template_id":"tmpl_pulled","boot_index_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","build_ref":"registry.example/template:latest"}}` + "\n"))
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONCH_API_URL", server.URL)

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stdout = writer
	err = runTemplatePull(context.Background(), []string{"-n", "team-a", "registry.example/template:latest"})
	_ = writer.Close()
	os.Stdout = oldStdout
	var output bytes.Buffer
	_, _ = output.ReadFrom(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatalf("runTemplatePull() error = %v", err)
	}
	if request.Reference != "registry.example/template:latest" || request.Namespace != "team-a" {
		t.Fatalf("request = %#v", request)
	}
	for _, want := range []string{"Pulling template:", "rootfs", "100.0%", "Template: tmpl_pulled"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, output.String())
		}
	}
}

func TestPrintTemplateHelpListsTemplateCommands(t *testing.T) {
	var buf bytes.Buffer
	printTemplateHelp(&buf)

	want := `Usage:
  conch template <command> [options]

Commands:
  create   Build a template from an OCI image, kernel, and initrd.
  pull     Pull a registry Boot Index into a local Template.
  push     Push a Template Boot Index to a registry.
  ls       List templates.
  inspect  Inspect a template.
  rm       Remove a template.

Run 'conch template <command> --help' for command-specific usage.
`
	if got := buf.String(); got != want {
		t.Fatalf("template help output mismatch:\n%s", got)
	}
}

func TestTemplateRegistryFlags(t *testing.T) {
	var pull templateRegistryOptions
	pullFlags := flag.NewFlagSet("template pull", flag.ContinueOnError)
	registerTemplateRegistryFlags(pullFlags, &pull, false)
	if err := pullFlags.Parse([]string{"--plain-http", "--user", "alice:secret", "-n", "team-a", "registry.example/template:latest"}); err != nil {
		t.Fatalf("parse pull flags: %v", err)
	}
	username, password, err := templateRegistryCredentials(pull)
	if err != nil {
		t.Fatalf("templateRegistryCredentials() error = %v", err)
	}
	if !pull.plainHTTP || pull.namespace != "team-a" || username != "alice" || password != "secret" || pullFlags.Arg(0) != "registry.example/template:latest" {
		t.Fatalf("pull options = %#v, args = %#v", pull, pullFlags.Args())
	}

	var push templateRegistryOptions
	pushFlags := flag.NewFlagSet("template push", flag.ContinueOnError)
	registerTemplateRegistryFlags(pushFlags, &push, true)
	if err := pushFlags.Parse([]string{"--username", "bob", "--password", "token", "--timeout", "5m", "-n", "team-a", "tmpl_1", "mirror.example/template:copy"}); err != nil {
		t.Fatalf("parse push flags: %v", err)
	}
	if push.username != "bob" || push.password != "token" || push.timeout != "5m" || push.namespace != "team-a" || len(pushFlags.Args()) != 2 {
		t.Fatalf("push options = %#v, args = %#v", push, pushFlags.Args())
	}
}

func TestPrintTemplatesAlignsRowsAndShowsEmptyFields(t *testing.T) {
	var buf bytes.Buffer
	printTemplates(&buf, []client.TemplateRecord{{
		ID:              "tmpl_ab2345da0a69b4e18aa24ad6",
		Origin:          "image",
		BootMode:        "cold",
		BootIndexDigest: "sha256:boot",
	}})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("template output lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	header, row := lines[0], lines[1]
	for _, column := range []struct {
		header string
		value  string
	}{
		{header: "ORIGIN", value: "image"},
		{header: "BOOT_MODE", value: "cold"},
		{header: "BOOT_INDEX_DIGEST", value: "sha256:boot"},
	} {
		if got, want := strings.Index(row, column.value), strings.Index(header, column.header); got != want {
			t.Errorf("%s column starts at %d, want %d:\n%s", column.header, got, want, buf.String())
		}
	}
	fields := strings.Fields(row)
	if len(fields) != 6 || fields[4] != "-" || fields[5] != "-" {
		t.Fatalf("template row fields = %#v, want two visible empty placeholders", fields)
	}
}
