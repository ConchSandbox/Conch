package cmd

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
  save     Save templates to a tar archive for offline transfer.
  load     Load templates from a tar archive.
  unpack   Unpack a Template's Boot Index into snapshots.
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
	const id = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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
	if err := pushFlags.Parse([]string{"--username", "bob", "--password", "token", "--timeout", "5m", id, "mirror.example/template:copy"}); err != nil {
		t.Fatalf("parse push flags: %v", err)
	}
	if push.username != "bob" || push.password != "token" || push.timeout != "5m" || len(pushFlags.Args()) != 2 {
		t.Fatalf("push options = %#v, args = %#v", push, pushFlags.Args())
	}
}

func TestTemplateArchiveFileReplacesOnlyAfterCommit(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "openeuler.tar")
	const original = "previous-archive"
	if err := os.WriteFile(dst, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveTemplateArchive(dst, func(w io.Writer) error {
		if _, err := io.WriteString(w, "partial"); err != nil {
			return err
		}
		return fmt.Errorf("save failed")
	}); err == nil {
		t.Fatal("expected save failure")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("archive after abort = %q, want %q", got, original)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".openeuler.tar.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}

	if err := saveTemplateArchive(dst, func(w io.Writer) error {
		_, err := io.WriteString(w, "new-archive")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-archive" {
		t.Fatalf("archive after commit = %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %#o, want 0600", info.Mode().Perm())
	}
}

func TestTemplateSaveLoadRejectsBadArgsBeforeAPI(t *testing.T) {
	if err := runTemplateSave(context.Background(), []string{"localhost/conch/demo:latest"}); err == nil || !strings.Contains(err.Error(), "-o or --output is required") {
		t.Fatalf("save without output: %v", err)
	}
	if err := runTemplateSave(context.Background(), []string{"-o", "out.tar"}); err == nil || !strings.Contains(err.Error(), "at least one Template Name") {
		t.Fatalf("save with no name: %v", err)
	}
	if err := runTemplateSave(context.Background(), []string{"-o", "out.tar", "--timeout", "nope", "localhost/conch/demo:latest"}); err == nil || !strings.Contains(err.Error(), "invalid --timeout") {
		t.Fatalf("save timeout: %v", err)
	}
	if err := runTemplateLoad(context.Background(), []string{"extra"}); err == nil || !strings.Contains(err.Error(), "unexpected positional") {
		t.Fatalf("load positional: %v", err)
	}
	if err := runTemplateLoad(context.Background(), []string{"--name", "other:latest"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("load --name: %v", err)
	}
	if err := runTemplateLoad(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "-i or --input is required") {
		t.Fatalf("load without input: %v", err)
	}
}

func TestPrintTemplatesAlignsRowsAndShowsEmptyFields(t *testing.T) {
	const id = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var buf bytes.Buffer
	printTemplates(&buf, []client.TemplateRecord{{
		Name:       "registry.example/conch/test:latest",
		Origin:     "image",
		BootMode:   "cold",
		TemplateID: id,
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
		{header: "NAME", value: "registry.example/conch/test:latest"},
		{header: "TEMPLATE_ID", value: id},
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
