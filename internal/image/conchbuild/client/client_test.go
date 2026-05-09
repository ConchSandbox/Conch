package client

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

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

func TestPauseSandboxIncludesNamespace(t *testing.T) {
	var got PauseRequest
	c := NewClient("http://example.invalid")
	c.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != pauseSandbox {
				t.Fatalf("path = %q, want %q", r.URL.Path, pauseSandbox)
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(`{"status":"ok","snapshotId":"sha256:test"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	snapshotID, err := c.PauseSandbox("sandbox-123", "team-a")
	if err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}
	if snapshotID != "sha256:test" {
		t.Fatalf("snapshotID = %q, want %q", snapshotID, "sha256:test")
	}
	if got.SandboxId != "sandbox-123" {
		t.Fatalf("sandbox_id = %q, want %q", got.SandboxId, "sandbox-123")
	}
	if got.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want %q", got.Namespace, "team-a")
	}
}
