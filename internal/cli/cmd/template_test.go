package cmd

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/cli/client"
)

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
	if err := pullFlags.Parse([]string{"--plain-http", "--user", "alice:secret", "registry.example/template:latest"}); err != nil {
		t.Fatalf("parse pull flags: %v", err)
	}
	username, password, err := templateRegistryCredentials(pull)
	if err != nil {
		t.Fatalf("templateRegistryCredentials() error = %v", err)
	}
	if !pull.plainHTTP || username != "alice" || password != "secret" || pullFlags.Arg(0) != "registry.example/template:latest" {
		t.Fatalf("pull options = %#v, args = %#v", pull, pullFlags.Args())
	}

	var push templateRegistryOptions
	pushFlags := flag.NewFlagSet("template push", flag.ContinueOnError)
	registerTemplateRegistryFlags(pushFlags, &push, true)
	if err := pushFlags.Parse([]string{"--username", "bob", "--password", "token", "--timeout", "5m", "tmpl_1", "mirror.example/template:copy"}); err != nil {
		t.Fatalf("parse push flags: %v", err)
	}
	if push.username != "bob" || push.password != "token" || push.timeout != "5m" || len(pushFlags.Args()) != 2 {
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
