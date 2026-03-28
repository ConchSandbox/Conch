package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintHelpIncludesBuildAndUnpack(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch build [buildah-bud-args...]",
		"conch unpack [options] <image-name>",
		"Subcommands:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("help output missing %q:\n%s", want, got)
		}
	}
}

func TestPrintUnpackHelpIncludesExample(t *testing.T) {
	var buf bytes.Buffer
	printUnpackHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch unpack [options] <image-name>",
		"containerd namespace",
		"containerd socket address",
		"conch unpack -n default hub.oepkgs.net/conch/conch-index:v0.1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("unpack help output missing %q:\n%s", want, got)
		}
	}
}

func TestPrintBuildHelpIncludesUsageAndEnv(t *testing.T) {
	var buf bytes.Buffer
	printBuildHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch build [buildah-bud-args...]",
		"Forward arguments to `buildah bud`",
		"CONCH_BUILDAH_BIN",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("build help output missing %q:\n%s", want, got)
		}
	}
}
