package cmd

import (
	"bytes"
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
  stop        Stop a sandbox.

Run 'conch sandbox <command> --help' for command-specific usage.
`
	if got := buf.String(); got != want {
		t.Fatalf("sandbox help output mismatch:\n%s", got)
	}
}
