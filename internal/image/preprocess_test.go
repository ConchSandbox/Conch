package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDockerfile(t *testing.T, dir, content string) string {
	t.Helper()

	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	return path
}

func TestPreprocessDockerfile_StripsExtensions(t *testing.T) {
	ctxDir := t.TempDir()
	for _, name := range []string{"vmlinuz", "conch.initrd"} {
		if err := os.WriteFile(filepath.Join(ctxDir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	dockerfile := writeDockerfile(t, ctxDir, "FROM scratch\nKERNEL vmlinuz conch.initrd\nRUN echo ok\nSNAP\n")

	res, err := PreprocessDockerfile(dockerfile, ctxDir)
	if err != nil {
		t.Fatalf("PreprocessDockerfile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(res.TempDockerfile) })

	if !res.Plan.NeedSnap {
		t.Fatal("expected SNAP to be detected")
	}
	if res.Plan.KernelFile != "vmlinuz" || res.Plan.InitrdFile != "conch.initrd" {
		t.Fatalf("unexpected kernel plan: %#v", res.Plan)
	}

	data, err := os.ReadFile(res.TempDockerfile)
	if err != nil {
		t.Fatalf("read temp Dockerfile: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "KERNEL") || strings.Contains(got, "SNAP") {
		t.Fatalf("temp Dockerfile still contains extension instructions:\n%s", got)
	}
}

func TestPreprocessDockerfile_StripsIndexExtension(t *testing.T) {
	ctxDir := t.TempDir()
	for _, name := range []string{"vmlinuz", "conch.initrd"} {
		if err := os.WriteFile(filepath.Join(ctxDir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	dockerfile := writeDockerfile(t, ctxDir, "FROM scratch\nKERNEL vmlinuz conch.initrd\nCOPY hello.txt /hello.txt\nINDEX\n")

	res, err := PreprocessDockerfile(dockerfile, ctxDir)
	if err != nil {
		t.Fatalf("PreprocessDockerfile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(res.TempDockerfile) })

	if !res.Plan.NeedIndex {
		t.Fatal("expected INDEX to be detected")
	}
	if res.Plan.NeedSnap {
		t.Fatal("did not expect SNAP to be detected")
	}
	if res.Plan.KernelFile != "vmlinuz" || res.Plan.InitrdFile != "conch.initrd" {
		t.Fatalf("unexpected kernel plan: %#v", res.Plan)
	}

	data, err := os.ReadFile(res.TempDockerfile)
	if err != nil {
		t.Fatalf("read temp Dockerfile: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "KERNEL") || strings.Contains(got, "INDEX") {
		t.Fatalf("temp Dockerfile still contains extension instructions:\n%s", got)
	}
}

func TestPreprocessDockerfile_SnapRequiresPrecedingKernel(t *testing.T) {
	ctxDir := t.TempDir()
	for _, name := range []string{"vmlinuz", "conch.initrd"} {
		if err := os.WriteFile(filepath.Join(ctxDir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	dockerfile := writeDockerfile(t, ctxDir, "FROM scratch\nSNAP\nKERNEL vmlinuz conch.initrd\n")

	_, err := PreprocessDockerfile(dockerfile, ctxDir)
	if err == nil {
		t.Fatal("expected SNAP-before-KERNEL to fail")
	}
	if !strings.Contains(err.Error(), "preceding KERNEL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreprocessDockerfile_IndexRequiresPrecedingKernel(t *testing.T) {
	ctxDir := t.TempDir()
	for _, name := range []string{"vmlinuz", "conch.initrd"} {
		if err := os.WriteFile(filepath.Join(ctxDir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	dockerfile := writeDockerfile(t, ctxDir, "FROM scratch\nINDEX\nKERNEL vmlinuz conch.initrd\n")

	_, err := PreprocessDockerfile(dockerfile, ctxDir)
	if err == nil {
		t.Fatal("expected INDEX-before-KERNEL to fail")
	}
	if !strings.Contains(err.Error(), "preceding KERNEL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreprocessDockerfile_RejectsIndexAndSnapTogether(t *testing.T) {
	ctxDir := t.TempDir()
	for _, name := range []string{"vmlinuz", "conch.initrd"} {
		if err := os.WriteFile(filepath.Join(ctxDir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	dockerfile := writeDockerfile(t, ctxDir, "FROM scratch\nKERNEL vmlinuz conch.initrd\nINDEX\nSNAP\n")

	_, err := PreprocessDockerfile(dockerfile, ctxDir)
	if err == nil {
		t.Fatal("expected INDEX+SNAP to fail")
	}
	if !strings.Contains(err.Error(), "INDEX and SNAP") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreprocessDockerfile_RejectsKernelPathTraversal(t *testing.T) {
	parentDir := t.TempDir()
	ctxDir := filepath.Join(parentDir, "context")
	if err := os.Mkdir(ctxDir, 0o755); err != nil {
		t.Fatalf("mkdir context: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parentDir, "outside.initrd"), []byte("initrd"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "vmlinuz"), []byte("kernel"), 0o644); err != nil {
		t.Fatalf("write kernel: %v", err)
	}

	dockerfile := writeDockerfile(t, ctxDir, "FROM scratch\nKERNEL vmlinuz ../outside.initrd\n")

	_, err := PreprocessDockerfile(dockerfile, ctxDir)
	if err == nil {
		t.Fatal("expected path traversal to fail")
	}
	if !strings.Contains(err.Error(), "escapes context directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}
