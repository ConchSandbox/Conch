package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveKernelPaths(t *testing.T) {
	tmpDir := t.TempDir()
	kernelFile := filepath.Join(tmpDir, "vmlinuz")
	initrdFile := filepath.Join(tmpDir, "conch.initrd")

	if err := os.WriteFile(kernelFile, []byte("kernel"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrdFile, []byte("initrd"), 0644); err != nil {
		t.Fatal(err)
	}

	kernelPath, diskPath, err := ResolveKernelPaths(tmpDir, "vmlinuz", "conch.initrd")
	if err != nil {
		t.Fatalf("ResolveKernelPaths: %v", err)
	}
	if kernelPath != kernelFile {
		t.Errorf("kernel path: got %s, want %s", kernelPath, kernelFile)
	}
	if diskPath != initrdFile {
		t.Errorf("disk path: got %s, want %s", diskPath, initrdFile)
	}
}

func TestResolveKernelPaths_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "vmlinuz"), []byte("x"), 0644)
	// conch.initrd does not exist

	_, _, err := ResolveKernelPaths(tmpDir, "vmlinuz", "conch.initrd")
	if err == nil {
		t.Error("expected error for missing initrd")
	}
}

func TestResolveKernelPaths_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()

	_, _, err := ResolveKernelPaths(tmpDir, "vmlinuz", "../../../etc/passwd")
	if err == nil {
		t.Error("expected error for path traversal")
	}
}
