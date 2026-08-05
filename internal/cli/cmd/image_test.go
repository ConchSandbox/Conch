package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/image/client"
)

func TestPrintImageHelpListsImageCommands(t *testing.T) {
	var buf bytes.Buffer
	printImageHelp(&buf)

	got := buf.String()
	for _, want := range []string{
		"conch image <command> [options]",
		"  pull ",
		"  push ",
		"  unpack ",
		"  ls ",
		"  rm ",
		"conch image <command> --help",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("image help output missing %q:\n%s", want, got)
		}
	}
}

func TestRunImageListPrintsKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/image/list" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"images":[{"name":"localhost/conch/demo:latest","target_digest":"sha256:demo","size":42,"kind":"boot-index-resume"}]}`))
	}))
	defer server.Close()

	t.Setenv("CONCH_API_URL", server.URL)
	w2Buf := &bytes.Buffer{}
	oldStdout := os.Stdout
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe2: %v", err)
	}
	os.Stdout = w2
	defer func() { os.Stdout = oldStdout }()

	if err := runImageList(context.Background(), nil); err != nil {
		t.Fatalf("runImageList() error = %v", err)
	}
	_ = w2.Close()
	if _, err := w2Buf.ReadFrom(r2); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}

	output := w2Buf.String()
	if !strings.HasPrefix(output, "NAME") {
		t.Fatalf("output should put NAME first:\n%s", output)
	}
	if !strings.Contains(output, "boot-index-resume") {
		t.Fatalf("output missing kind value:\n%s", output)
	}
}

func TestRunImageListRejectsInvalidConfig(t *testing.T) {
	configPath := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(configPath, []byte("server: [\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("CONCH_API_URL", "http://127.0.0.1:4063")

	err := runImageList(context.Background(), []string{"--config", configPath})
	if err == nil {
		t.Fatal("runImageList() error = nil")
	}
	if !strings.Contains(err.Error(), "failed to parse config file") {
		t.Fatalf("runImageList() error = %v, want YAML parse error", err)
	}
}

func TestPrintImageListHidesInternalRecordsByDefault(t *testing.T) {
	images := []client.ImageRecord{
		{Name: "localhost:5000/busybox:latest", TargetDigest: "sha256:source"},
		{Name: "localhost/conch/busybox:latest", TargetDigest: "sha256:index", Kind: "boot-index-cold"},
		{Name: "conch-erofs-rootfs:tmpl-1", TargetDigest: "sha256:rootfs"},
		{Name: "localhost/conch/rootfs-component:rootfs", TargetDigest: "sha256:rootfs", Kind: "boot-component-rootfs"},
		{Name: "localhost/conch/sandbox-component:sandbox", TargetDigest: "sha256:sandbox", Kind: "boot-component-sandbox"},
		{Name: "localhost/conch/mem-snapshot-component:mem", TargetDigest: "sha256:mem", Kind: "boot-component-memory"},
	}

	var visible bytes.Buffer
	if err := printImageList(&visible, images, false); err != nil {
		t.Fatalf("printImageList(default) error = %v", err)
	}
	for _, want := range []string{"localhost:5000/busybox:latest", "localhost/conch/busybox:latest"} {
		if !strings.Contains(visible.String(), want) {
			t.Fatalf("default output missing %q:\n%s", want, visible.String())
		}
	}
	for _, hidden := range []string{"conch-erofs-rootfs:", "rootfs-component", "sandbox-component", "mem-snapshot-component"} {
		if strings.Contains(visible.String(), hidden) {
			t.Fatalf("default output unexpectedly contains %q:\n%s", hidden, visible.String())
		}
	}

	var all bytes.Buffer
	if err := printImageList(&all, images, true); err != nil {
		t.Fatalf("printImageList(all) error = %v", err)
	}
	for _, want := range []string{"conch-erofs-rootfs:", "rootfs-component", "sandbox-component", "mem-snapshot-component"} {
		if !strings.Contains(all.String(), want) {
			t.Fatalf("--all output missing %q:\n%s", want, all.String())
		}
	}
}

func TestDisplayImageKindDefaultsToOCIImage(t *testing.T) {
	if got := displayImageKind(""); got != "oci-image" {
		t.Fatalf("displayImageKind(empty) = %q, want oci-image", got)
	}
}
