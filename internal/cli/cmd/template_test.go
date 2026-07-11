package cmd

import (
	"bytes"
	"testing"
)

func TestPrintTemplateHelpListsTemplateCommands(t *testing.T) {
	var buf bytes.Buffer
	printTemplateHelp(&buf)

	want := `Usage:
  conch template <command> [options]

Commands:
  create   Build a template from an OCI image, kernel, and initrd.
  ls       List templates.
  inspect  Inspect a template.
  rm       Remove a template.

Run 'conch template <command> --help' for command-specific usage.
`
	if got := buf.String(); got != want {
		t.Fatalf("template help output mismatch:\n%s", got)
	}
}
