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
		"--config string",
		"CONCH_BUILDAH_BIN",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("build help output missing %q:\n%s", want, got)
		}
	}
}

func TestParseBuildConfigArg(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantConfig string
		wantArgs   []string
		wantErr    bool
	}{
		{
			name:       "long separate",
			args:       []string{"--config", "/tmp/config.yaml", "-f", "Dockerfile", "-t", "demo:latest", "."},
			wantConfig: "/tmp/config.yaml",
			wantArgs:   []string{"-f", "Dockerfile", "-t", "demo:latest", "."},
		},
		{
			name:       "long equals",
			args:       []string{"--config=/tmp/config.yaml", "-f", "Dockerfile", "."},
			wantConfig: "/tmp/config.yaml",
			wantArgs:   []string{"-f", "Dockerfile", "."},
		},
		{
			name:       "short equals",
			args:       []string{"-config=/tmp/config.yaml", "."},
			wantConfig: "/tmp/config.yaml",
			wantArgs:   []string{"."},
		},
		{
			name:    "missing value",
			args:    []string{"--config"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotConfig, gotArgs, err := parseBuildConfigArg(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseBuildConfigArg() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if gotConfig != tt.wantConfig {
				t.Fatalf("config: got %q want %q", gotConfig, tt.wantConfig)
			}
			if strings.Join(gotArgs, "\x00") != strings.Join(tt.wantArgs, "\x00") {
				t.Fatalf("args: got %#v want %#v", gotArgs, tt.wantArgs)
			}
		})
	}
}
