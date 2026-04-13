package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestPrintHelpIncludesBuildPullAndUnpack(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch build [buildah-bud-args...]",
		"conch pull [options] <image-name>",
		"conch unpack [options] <image-name>",
		"Subcommands:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("help output missing %q:\n%s", want, got)
		}
	}
}

func TestPrintPullHelpIncludesExample(t *testing.T) {
	var buf bytes.Buffer
	printPullHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch pull [options] <image-name>",
		"containerd namespace",
		"config file path",
		"--plain-http",
		"--user string",
		"--kernel-plain-http",
		"--kernel-user string",
		"hub.oepkgs.net/conch/sandbox-snapshot:latest",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("pull help output missing %q:\n%s", want, got)
		}
	}
}

func TestParseRegistryUser(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantUser string
		wantPass string
		wantErr  bool
	}{
		{name: "empty", input: "", wantUser: "", wantPass: ""},
		{name: "valid", input: "example-user:example-password", wantUser: "example-user", wantPass: "example-password"},
		{name: "missing colon", input: "conch", wantErr: true},
		{name: "missing password", input: "conch:", wantErr: true},
		{name: "missing username", input: ":secret", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, pass, err := parseRegistryUser(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRegistryUser() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if user != tt.wantUser || pass != tt.wantPass {
				t.Fatalf("parseRegistryUser() = (%q, %q), want (%q, %q)", user, pass, tt.wantUser, tt.wantPass)
			}
		})
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
		"config file path",
		"conch unpack -n default hub.oepkgs.net/conch/conch-index:v0.1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("unpack help output missing %q:\n%s", want, got)
		}
	}
}

func TestResolveContainerdRuntimeUsesConfigAndOverrides(t *testing.T) {
	t.Setenv("CONTAINERD_ADDRESS", "")

	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	if err := os.WriteFile(cfgPath, []byte(`
containerd:
  socket: /run/containerd/custom.sock
  default_namespace: team-a
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConchConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	addr, ns := resolveContainerdRuntime(cfg, "", "")
	if addr != "/run/containerd/custom.sock" {
		t.Fatalf("addr = %q, want %q", addr, "/run/containerd/custom.sock")
	}
	if ns != "team-a" {
		t.Fatalf("namespace = %q, want %q", ns, "team-a")
	}

	addr, ns = resolveContainerdRuntime(cfg, "/run/containerd/override.sock", "override-ns")
	if addr != "/run/containerd/override.sock" {
		t.Fatalf("override addr = %q, want %q", addr, "/run/containerd/override.sock")
	}
	if ns != "override-ns" {
		t.Fatalf("override namespace = %q, want %q", ns, "override-ns")
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
