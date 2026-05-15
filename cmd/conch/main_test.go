package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestPrintHelpIncludesBuildPushPullUnpackAndSnapshot(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)

	got := buf.String()
	if strings.Contains(got, "CONTAINERD_ADDRESS") {
		t.Fatalf("help output still references CONTAINERD_ADDRESS:\n%s", got)
	}
	for _, want := range []string{
		"conch build [buildah-bud-args...]",
		"conch push [options] <local-image> <remote-image>",
		"conch pull [options] <image-name>",
		"conch unpack [options] <image-name>",
		"conch snapshot export [options]",
		"Subcommands:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("help output missing %q:\n%s", want, got)
		}
	}
}

func TestPrintPushHelpIncludesExample(t *testing.T) {
	var buf bytes.Buffer
	printPushHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch push [options] <local-image> <remote-image>",
		"buildah manifest push --all",
		"--plain-http",
		"--tls-verify bool",
		"buildah login",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("push help output missing %q:\n%s", want, got)
		}
	}
}

func TestParsePushArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantLocal  string
		wantRemote string
		wantTLS    bool
		wantErr    bool
	}{
		{
			name:       "default",
			args:       []string{"localhost/demo:latest", "hub.oepkgs.net/conch/demo:latest"},
			wantLocal:  "localhost/demo:latest",
			wantRemote: "hub.oepkgs.net/conch/demo:latest",
			wantTLS:    true,
		},
		{
			name:       "plain http",
			args:       []string{"--plain-http", "localhost/demo:latest", "conch.example.com/conch/demo:latest"},
			wantLocal:  "localhost/demo:latest",
			wantRemote: "conch.example.com/conch/demo:latest",
			wantTLS:    false,
		},
		{
			name:       "tls verify false",
			args:       []string{"--tls-verify=false", "localhost/demo:latest", "conch.example.com/conch/demo:latest"},
			wantLocal:  "localhost/demo:latest",
			wantRemote: "conch.example.com/conch/demo:latest",
			wantTLS:    false,
		},
		{
			name:    "missing image",
			args:    []string{"localhost/demo:latest"},
			wantErr: true,
		},
		{
			name:    "unknown option",
			args:    []string{"--user", "demo:demo", "localhost/demo:latest", "remote/demo:latest"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePushArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePushArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.localImage != tt.wantLocal || got.remoteImage != tt.wantRemote || got.tlsVerify != tt.wantTLS {
				t.Fatalf("parsePushArgs() = %#v", got)
			}
		})
	}
}

func TestBuildPushCommandArgs(t *testing.T) {
	got := buildPushCommandArgs(pushOptions{
		localImage:  "localhost/demo:latest",
		remoteImage: "conch.example.com/conch/demo:latest",
		tlsVerify:   false,
	})
	want := []string{
		"manifest", "push", "--all", "--tls-verify=false",
		"localhost/demo:latest",
		"docker://conch.example.com/conch/demo:latest",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("buildPushCommandArgs() = %#v, want %#v", got, want)
	}
}

func TestPrintPullHelpIncludesExample(t *testing.T) {
	var buf bytes.Buffer
	printPullHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch pull [options] <image-name>",
		"containerd namespace",
		"conchd API base URL",
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
		"conchd API base URL",
		"config file path",
		"conch unpack -n default hub.oepkgs.net/conch/conch-index:v0.1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("unpack help output missing %q:\n%s", want, got)
		}
	}
}

func TestResolveConchNamespaceUsesConfigAndOverride(t *testing.T) {
	t.Setenv("CONTAINERD_ADDRESS", "unix:///must/not/be/used.sock")

	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	if err := os.WriteFile(cfgPath, []byte(`
containerd:
  default_namespace: team-a
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConchConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	ns := resolveConchNamespace(cfg, "")
	if ns != "team-a" {
		t.Fatalf("namespace = %q, want %q", ns, "team-a")
	}

	ns = resolveConchNamespace(cfg, "override-ns")
	if ns != "override-ns" {
		t.Fatalf("override namespace = %q, want %q", ns, "override-ns")
	}

	if got := resolveConchAPIURL("http://explicit", "http://alias"); got != "http://explicit" {
		t.Fatalf("api url = %q, want explicit", got)
	}
	if got := resolveConchAPIURL("", "http://alias"); got != "http://alias" {
		t.Fatalf("api url alias = %q, want alias", got)
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
		"`KERNEL`, `INDEX`, and `SNAP`",
		"CONCH_BUILDAH_BIN",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("build help output missing %q:\n%s", want, got)
		}
	}
}

func TestPrintSnapshotHelpIncludesExport(t *testing.T) {
	var buf bytes.Buffer
	printSnapshotHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch snapshot export [options]",
		"export  Export a sandbox-snapshot image",
		"conch snapshot export --snapshot-id <rootfs-snapshot-id> -t <sandbox-snapshot-image>",
		"conch snapshot export --sandbox-id <sandbox-id> -t <sandbox-snapshot-image>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("snapshot help output missing %q:\n%s", want, got)
		}
	}
}

func TestPrintSnapshotExportHelpIncludesExamples(t *testing.T) {
	var buf bytes.Buffer
	fs, _ := newSnapshotExportFlagSet(&buf)
	printSnapshotExportHelp(&buf, fs)

	got := buf.String()
	for _, want := range []string{
		"-snapshot-id string",
		"-sandbox-id string",
		"-tag string",
		"-t string",
		"Either one of --snapshot-id or --sandbox-id is required.",
		"conch snapshot export --snapshot-id sha256:abc -t localhost/conch/sandbox-snapshot:latest",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("snapshot export help output missing %q:\n%s", want, got)
		}
	}
}

func TestParseSnapshotExportArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantSnapshot string
		wantSandbox  string
		wantTag      string
		wantConfig   string
		wantErrText  string
		wantErr      bool
	}{
		{
			name:         "snapshot id",
			args:         []string{"--snapshot-id", "sha256:abc", "-t", "localhost/conch/sandbox-snapshot:latest"},
			wantSnapshot: "sha256:abc",
			wantTag:      "localhost/conch/sandbox-snapshot:latest",
		},
		{
			name:        "sandbox id with config",
			args:        []string{"--sandbox-id", "sandbox-123", "--tag", "localhost/conch/demo:latest", "--config", "/tmp/config.yaml"},
			wantSandbox: "sandbox-123",
			wantTag:     "localhost/conch/demo:latest",
			wantConfig:  "/tmp/config.yaml",
		},
		{
			name:        "missing tag",
			args:        []string{"--snapshot-id", "sha256:abc"},
			wantErr:     true,
			wantErrText: "output tag is required",
		},
		{
			name:        "missing source",
			args:        []string{"-t", "localhost/conch/demo:latest"},
			wantErr:     true,
			wantErrText: "exactly one of --snapshot-id or --sandbox-id is required",
		},
		{
			name:        "both sources",
			args:        []string{"--snapshot-id", "sha256:abc", "--sandbox-id", "sandbox-123", "-t", "localhost/conch/demo:latest"},
			wantErr:     true,
			wantErrText: "exactly one of --snapshot-id or --sandbox-id is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSnapshotExportArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSnapshotExportArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("parseSnapshotExportArgs() error = %q, want substring %q", err.Error(), tt.wantErrText)
				}
				return
			}
			if got.snapshotID != tt.wantSnapshot || got.sandboxID != tt.wantSandbox || got.tag != tt.wantTag || got.configPath != tt.wantConfig {
				t.Fatalf("parseSnapshotExportArgs() = %#v", got)
			}
		})
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
