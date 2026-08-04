package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	cmd "github.com/openeuler/Conch/internal/cli/cmd"
	"github.com/openeuler/Conch/internal/image/client"
)

func TestPrintHelpListsSubcommands(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)

	got := buf.String()
	if strings.Contains(got, "CONTAINERD_ADDRESS") {
		t.Fatalf("help output still references CONTAINERD_ADDRESS:\n%s", got)
	}
	for _, want := range []string{
		"conch <command> [options]",
		"Commands:",
		"  image ",
		"  sandbox ",
		"  template ",
		"  debug ",
		"conch <command> --help",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("help output missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"CONCH_API_TIMEOUT",
		"CONCH_REGISTRY_TIMEOUT",
		"CONCH_API_URL",
		"conch image push [options]",
		"conch sandbox create",
		"conch sandbox checkpoint",
		"conch template create",
		"conch debug snapshot",
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("help output should not expose %q:\n%s", unwanted, got)
		}
	}
	if strings.Contains(got, "conch snapshots ls [options]") {
		t.Fatalf("help output still contains deprecated snapshots command:\n%s", got)
	}
	if strings.Contains(got, "snapshot export") {
		t.Fatalf("help should not expose removed snapshot export command:\n%s", got)
	}
	if strings.Contains(got, "conch convert") {
		t.Fatalf("help output still contains removed convert command:\n%s", got)
	}
}

func TestPrintHelpAlignsCommandDescriptions(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)

	for _, want := range []string{
		"  image     Pull, push, unpack, list, or remove images.",
		"  sandbox   Create, checkpoint, or control sandboxes from Template IDs.",
		"  template  Build, list, inspect, or remove templates.",
		"  debug     Low-level inspection and repair commands.",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("help output missing aligned command line %q:\n%s", want, buf.String())
		}
	}
}

func TestRunRejectsRemovedTopLevelCommands(t *testing.T) {
	for _, command := range []string{"pull", "push", "unpack", "snapshot", "snapshots", "checkpoint", "convert"} {
		t.Run(command, func(t *testing.T) {
			oldStderr := os.Stderr
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			os.Stderr = w
			code := Run([]string{command, "--help"})
			_ = w.Close()
			os.Stderr = oldStderr

			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r)
			if code != 2 {
				t.Fatalf("Run(%q --help) exit code = %d, want 2; stderr:\n%s", command, code, buf.String())
			}
			if !strings.Contains(buf.String(), "unknown command "+strconv.Quote(command)) {
				t.Fatalf("Run(%q --help) stderr missing unknown command:\n%s", command, buf.String())
			}
		})
	}
}

func TestPrintImagePushHelpIncludesExample(t *testing.T) {
	var buf bytes.Buffer
	cmd.PrintImagePushHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch image push [options] <local-image> <remote-image>",
		"conchd/containerd",
		"--plain-http",
		"--username string",
		"--timeout duration",
		"timeout for this push operation",
		"conch image push --timeout 30m",
		"containerd namespace",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("push help output missing %q:\n%s", want, got)
		}
	}
}

func TestPrintSandboxCreateHelpExplainsDaemonDefaults(t *testing.T) {
	var buf bytes.Buffer
	cmd.PrintSandboxCreateHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch sandbox create --template-id <template-id> [options]",
		"VMM and vCPU settings are selected",
		"memory also uses the conchd default unless --ram-mb is set",
		"conchd sandbox.default_ram_mb",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sandbox create help missing %q:\n%s", want, got)
		}
	}
}

func TestParseImagePushArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantLocal   string
		wantRemote  string
		wantPlain   bool
		wantNS      string
		wantTimeout string
		wantErr     bool
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
			name:        "timeout",
			args:        []string{"--timeout=10m", "localhost/demo:latest", "conch.example.com/conch/demo:latest"},
			wantLocal:   "localhost/demo:latest",
			wantRemote:  "conch.example.com/conch/demo:latest",
			wantTimeout: "10m",
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
			got, err := cmd.ParseImagePushArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePushArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.LocalImage != tt.wantLocal || got.RemoteImage != tt.wantRemote || got.PlainHTTP != tt.wantPlain || got.Namespace != tt.wantNS || got.Timeout != tt.wantTimeout {
				t.Fatalf("parsePushArgs() = %#v", got)
			}
		})
	}
}

func TestRunImagePushPassesResolvedNamespace(t *testing.T) {
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

	err := cmd.RunImagePush(context.Background(), []string{"--config", cfgPath, "localhost/demo:latest", "remote/demo:latest"})
	if err != nil {
		t.Fatalf("runPush: %v", err)
	}
	if got.LocalImage != "localhost/demo:latest" || got.RemoteImage != "remote/demo:latest" {
		t.Fatalf("push request = %#v", got)
	}
	if got.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want %q", got.Namespace, "team-a")
	}
	if got.RegistryTimeout != "" {
		t.Fatalf("registry timeout = %q, want empty", got.RegistryTimeout)
	}
}

func TestRunImagePushPassesTimeout(t *testing.T) {
	var got client.PushImageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode push request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()
	t.Setenv("CONCH_API_URL", server.URL)

	err := cmd.RunImagePush(context.Background(), []string{"--timeout", "30m", "localhost/demo:latest", "remote/demo:latest"})
	if err != nil {
		t.Fatalf("runPush: %v", err)
	}
	if got.RegistryTimeout != "30m" {
		t.Fatalf("registry timeout = %q, want 30m", got.RegistryTimeout)
	}
}

func TestPrintImagePullHelpIncludesExample(t *testing.T) {
	var buf bytes.Buffer
	cmd.PrintImagePullHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch image pull [options] <image-name>",
		"containerd namespace",
		"conchd API base URL",
		"config file path",
		"--plain-http",
		"--user string",
		"--skip-unpack",
		"hub.oepkgs.net/conch/sandbox-snapshot:latest",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("pull help output missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"--kernel-plain-http", "--kernel-user"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("pull help output should not expose %q:\n%s", unwanted, got)
		}
	}
}

func TestRunImagePullPassesSkipUnpack(t *testing.T) {
	var got client.PullImageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/image/pull" {
			t.Fatalf("path = %q, want /api/image/pull", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode pull request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": map[string]string{}})
	}))
	defer server.Close()
	t.Setenv("CONCH_API_URL", server.URL)

	err := cmd.RunImagePull(context.Background(), []string{"--skip-unpack", "docker.io/library/nginx:latest"})
	if err != nil {
		t.Fatalf("RunImagePull() error = %v", err)
	}
	if got.ImageName != "docker.io/library/nginx:latest" || !got.SkipUnpack {
		t.Fatalf("pull request = %#v", got)
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
			user, pass, err := cmd.ParseRegistryUser(tt.input)
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

func TestPrintImageUnpackHelpIncludesExample(t *testing.T) {
	var buf bytes.Buffer
	cmd.PrintImageUnpackHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch image unpack [options] <image-name>",
		"containerd namespace",
		"conchd API base URL",
		"config file path",
		"conch image unpack -n default hub.oepkgs.net/conch/conch-index:v0.1",
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

	cfg, err := cmd.LoadConchConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	ns := cmd.ResolveConchNamespace(cfg, "")
	if ns != "team-a" {
		t.Fatalf("namespace = %q, want %q", ns, "team-a")
	}

	ns = cmd.ResolveConchNamespace(cfg, "override-ns")
	if ns != "override-ns" {
		t.Fatalf("override namespace = %q, want %q", ns, "override-ns")
	}

	if got := cmd.ResolveConchAPIURL("http://explicit", "http://alias"); got != "http://explicit" {
		t.Fatalf("api url = %q, want explicit", got)
	}
	if got := cmd.ResolveConchAPIURL("", "http://alias"); got != "http://alias" {
		t.Fatalf("api url alias = %q, want alias", got)
	}
}

func TestPrintTemplateCreateHelpIncludesUsageAndInputs(t *testing.T) {
	var buf bytes.Buffer
	cmd.PrintTemplateCreateHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch template create --source <image> --kernel <path> --initrd <path> [options]",
		"--source string",
		"--kernel string",
		"--initrd string",
		"--user string",
		"--username string",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("template create help output missing %q:\n%s", want, got)
		}
	}
}

func TestRunTemplateCreateUsesTemplateCreateAPI(t *testing.T) {
	dir := t.TempDir()
	kernelPath := dir + "/vmlinuz"
	initrdPath := dir + "/conch.initrd"
	cfgPath := dir + "/config.yaml"
	if err := os.WriteFile(kernelPath, []byte("kernel-content"), 0o644); err != nil {
		t.Fatalf("write kernel: %v", err)
	}
	if err := os.WriteFile(initrdPath, []byte("initrd-content"), 0o644); err != nil {
		t.Fatalf("write initrd: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(`
containerd:
  default_namespace: team-a
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var metadata client.TemplateCreateMetadata
	var kernelBody string
	var initrdBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/template/create" {
			t.Fatalf("path = %q, want /api/template/create", r.URL.Path)
		}
		if err := r.ParseMultipartForm(4096); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		if err := json.Unmarshal([]byte(r.FormValue("metadata")), &metadata); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
		file, _, err := r.FormFile("kernel")
		if err != nil {
			t.Fatalf("kernel FormFile: %v", err)
		}
		raw, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			t.Fatalf("ReadAll kernel: %v", err)
		}
		kernelBody = string(raw)
		file, _, err = r.FormFile("initrd")
		if err != nil {
			t.Fatalf("initrd FormFile: %v", err)
		}
		raw, err = io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			t.Fatalf("ReadAll initrd: %v", err)
		}
		initrdBody = string(raw)
		_ = json.NewEncoder(w).Encode(client.TemplateCreateResponse{
			Status:          "ok",
			TemplateID:      "tmpl_123",
			BootIndexDigest: "sha256:template",
			BootIndexTag:    "localhost/conch/busybox:latest",
		})
	}))
	defer server.Close()

	err := cmd.RunTemplateCreate(context.Background(), []string{
		"--config", cfgPath,
		"--api-url", server.URL,
		"--source", "public.ecr.aws/docker/library/busybox:latest",
		"--kernel", kernelPath,
		"--initrd", initrdPath,
		"-t", "localhost/conch/busybox:latest",
	})
	if err != nil {
		t.Fatalf("runTemplateCreate: %v", err)
	}
	if metadata.Source != "public.ecr.aws/docker/library/busybox:latest" || metadata.Namespace != "team-a" || metadata.BootIndexTag != "localhost/conch/busybox:latest" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if kernelBody != "kernel-content" || initrdBody != "initrd-content" {
		t.Fatalf("uploaded bodies kernel=%q initrd=%q", kernelBody, initrdBody)
	}
}

func TestPrintSnapshotHelpUsesDebugCommand(t *testing.T) {
	var buf bytes.Buffer
	cmd.PrintSnapshotHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch debug snapshot ls [options]",
		"conch debug snapshot rm [options] <snapshot-key>",
		"ls      List EROFS snapshots",
		"rm      Remove one EROFS snapshot",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("snapshot help output missing %q:\n%s", want, got)
		}
	}
	for _, removed := range []string{"conch snapshot ls", "conch snapshots ls"} {
		if strings.Contains(got, removed) {
			t.Fatalf("snapshot help output still contains removed top-level command %q:\n%s", removed, got)
		}
	}
}
