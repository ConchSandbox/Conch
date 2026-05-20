package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/image/client"
)

func TestPrintHelpIncludesConvertPushPullUnpackAndSnapshot(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)

	got := buf.String()
	if strings.Contains(got, "CONTAINERD_ADDRESS") {
		t.Fatalf("help output still references CONTAINERD_ADDRESS:\n%s", got)
	}
	for _, want := range []string{
		"conch convert [options]",
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
		"conchd/containerd",
		"--plain-http",
		"--username string",
		"containerd namespace",
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
		wantPlain  bool
		wantNS     string
		wantErr    bool
	}{
		{
			name:       "default",
			args:       []string{"localhost/demo:latest", "hub.oepkgs.net/conch/demo:latest"},
			wantLocal:  "localhost/demo:latest",
			wantRemote: "hub.oepkgs.net/conch/demo:latest",
		},
		{
			name:       "plain http",
			args:       []string{"--plain-http", "-n", "team-a", "localhost/demo:latest", "conch.example.com/conch/demo:latest"},
			wantLocal:  "localhost/demo:latest",
			wantRemote: "conch.example.com/conch/demo:latest",
			wantPlain:  true,
			wantNS:     "team-a",
		},
		{
			name:       "namespace equals",
			args:       []string{"--namespace=team-b", "localhost/demo:latest", "conch.example.com/conch/demo:latest"},
			wantLocal:  "localhost/demo:latest",
			wantRemote: "conch.example.com/conch/demo:latest",
			wantNS:     "team-b",
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
			if got.localImage != tt.wantLocal || got.remoteImage != tt.wantRemote || got.plainHTTP != tt.wantPlain || got.namespace != tt.wantNS {
				t.Fatalf("parsePushArgs() = %#v", got)
			}
		})
	}
}

func TestRunPushPassesResolvedNamespace(t *testing.T) {
	var got client.PushImageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/image/push" {
			t.Fatalf("path = %q, want /api/image/push", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode push request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()
	t.Setenv("CONCH_API_URL", server.URL)

	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	if err := os.WriteFile(cfgPath, []byte(`
containerd:
  default_namespace: team-a
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := runPush(context.Background(), []string{"--config", cfgPath, "localhost/demo:latest", "remote/demo:latest"})
	if err != nil {
		t.Fatalf("runPush: %v", err)
	}
	if got.LocalImage != "localhost/demo:latest" || got.RemoteImage != "remote/demo:latest" {
		t.Fatalf("push request = %#v", got)
	}
	if got.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want %q", got.Namespace, "team-a")
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

func TestPrintConvertHelpIncludesUsageAndInputs(t *testing.T) {
	var buf bytes.Buffer
	printConvertHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch convert [options]",
		"--source string",
		"--kernel string",
		"--initrd string",
		"--snapshot",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("convert help output missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"--archive", "--source-tag", "--source-image"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("convert help output still contains %q:\n%s", unwanted, got)
		}
	}
}

func TestParseConvertArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    convertOptions
		wantErr bool
	}{
		{
			name: "registry source",
			args: []string{"--source", "docker.io/library/nginx:latest", "--kernel", "./bzImage", "--initrd", "./conch.initrd", "-t", "localhost/conch/nginx:latest"},
			want: convertOptions{source: "docker.io/library/nginx:latest", kernel: "./bzImage", initrd: "./conch.initrd", tag: "localhost/conch/nginx:latest"},
		},
		{
			name:    "missing source",
			args:    []string{"--kernel", "./bzImage", "--initrd", "./conch.initrd", "-t", "localhost/conch/demo:latest"},
			wantErr: true,
		},
		{
			name:    "source image unsupported",
			args:    []string{"--source-image", "localhost/source:latest", "--kernel", "./bzImage", "--initrd", "./conch.initrd", "-t", "localhost/conch/demo:latest"},
			wantErr: true,
		},
		{
			name:    "archive source unsupported",
			args:    []string{"--archive", "./rootfs.oci.tar", "--kernel", "./bzImage", "--initrd", "./conch.initrd", "-t", "localhost/conch/demo:latest"},
			wantErr: true,
		},
		{
			name:    "missing kernel",
			args:    []string{"--source", "nginx:latest", "--initrd", "./conch.initrd", "-t", "localhost/conch/demo:latest"},
			wantErr: true,
		},
		{
			name:    "missing initrd",
			args:    []string{"--source", "nginx:latest", "--kernel", "./bzImage", "-t", "localhost/conch/demo:latest"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConvertArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseConvertArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.source != tt.want.source || got.kernel != tt.want.kernel || got.initrd != tt.want.initrd || got.tag != tt.want.tag || got.snapshot != tt.want.snapshot {
				t.Fatalf("parseConvertArgs() = %#v, want %#v", got, tt.want)
			}
		})
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
		"-namespace string",
		"-n string",
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
		wantNS       string
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
			name:        "sandbox id with config and namespace",
			args:        []string{"--sandbox-id", "sandbox-123", "--tag", "localhost/conch/demo:latest", "--config", "/tmp/config.yaml", "-n", "team-a"},
			wantSandbox: "sandbox-123",
			wantTag:     "localhost/conch/demo:latest",
			wantConfig:  "/tmp/config.yaml",
			wantNS:      "team-a",
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
			if got.snapshotID != tt.wantSnapshot || got.sandboxID != tt.wantSandbox || got.tag != tt.wantTag || got.configPath != tt.wantConfig || got.namespace != tt.wantNS {
				t.Fatalf("parseSnapshotExportArgs() = %#v", got)
			}
		})
	}
}
