package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestImageAPIMethods(t *testing.T) {
	var pullReq PullImageRequest
	var unpackReq UnpackImageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pullImage:
			if err := json.NewDecoder(r.Body).Decode(&pullReq); err != nil {
				t.Fatalf("decode pull request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(ImageResponse{Results: map[string]string{"rootfs": "rootfs-id"}})
		case unpackImage:
			if err := json.NewDecoder(r.Body).Decode(&unpackReq); err != nil {
				t.Fatalf("decode unpack request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(ImageResponse{Results: map[string]string{"sandbox": "sandbox-id"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL)
	pullResults, err := c.PullImage(context.Background(), PullImageRequest{
		ImageName:          "docker.io/library/nginx:latest",
		Namespace:          "team-a",
		DefaultKernelImage: "hub.oepkgs.net/conch/kernel:6.6.0",
	})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if pullReq.ImageName != "docker.io/library/nginx:latest" || pullReq.Namespace != "team-a" {
		t.Fatalf("pull request = %#v", pullReq)
	}
	if pullResults["rootfs"] != "rootfs-id" {
		t.Fatalf("pull results = %#v", pullResults)
	}

	unpackResults, err := c.UnpackImage(context.Background(), UnpackImageRequest{
		ImageName: "hub.oepkgs.net/conch/conch-index:v0.1",
		Namespace: "default",
	})
	if err != nil {
		t.Fatalf("UnpackImage: %v", err)
	}
	if unpackReq.ImageName != "hub.oepkgs.net/conch/conch-index:v0.1" || unpackReq.Namespace != "default" {
		t.Fatalf("unpack request = %#v", unpackReq)
	}
	if unpackResults["sandbox"] != "sandbox-id" {
		t.Fatalf("unpack results = %#v", unpackResults)
	}
}

func TestImageAPIErrorIncludesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad image", http.StatusBadRequest)
	}))
	defer server.Close()

	c := NewClient(server.URL)
	_, err := c.PullImage(context.Background(), PullImageRequest{ImageName: "bad"})
	if err == nil {
		t.Fatal("PullImage() error = nil")
	}
	if !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("error = %v, want status 400", err)
	}
}

func TestConchAPITimeoutEnv(t *testing.T) {
	t.Setenv("CONCH_API_TIMEOUT", "5m")
	c := NewClient("http://127.0.0.1:4063")
	if c.httpClient.Timeout != 5*time.Minute {
		t.Fatalf("timeout = %s, want 5m", c.httpClient.Timeout)
	}

	t.Setenv("CONCH_API_TIMEOUT", "bad")
	c = NewClient("http://127.0.0.1:4063")
	if c.httpClient.Timeout != defaultHTTPTimeout {
		t.Fatalf("timeout = %s, want default %s", c.httpClient.Timeout, defaultHTTPTimeout)
	}
}

func TestImportAndSnapshotAPIMethods(t *testing.T) {
	archive, err := os.CreateTemp(t.TempDir(), "image-*.tar")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := archive.WriteString("archive-content"); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}

	var linkReq LinkSnapshotVMRequest
	var infoReq SnapshotInfoRequest
	var chainReq SnapshotInfoRequest
	var importedTag string
	var importedNamespace string
	var archiveBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case importImage:
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Fatalf("ParseMultipartForm: %v", err)
			}
			importedTag = r.FormValue("imported_tag")
			importedNamespace = r.FormValue("namespace")
			file, _, err := r.FormFile("archive")
			if err != nil {
				t.Fatalf("FormFile: %v", err)
			}
			defer file.Close()
			raw, err := io.ReadAll(file)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			archiveBody = string(raw)
			_ = json.NewEncoder(w).Encode(ImportImageResponse{SnapshotKey: "snapshot-id", ImageName: "image:latest"})
		case linkSnapshotVM:
			if err := json.NewDecoder(r.Body).Decode(&linkReq); err != nil {
				t.Fatalf("decode link request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case snapshotInfo:
			if err := json.NewDecoder(r.Body).Decode(&infoReq); err != nil {
				t.Fatalf("decode info request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(SnapshotMeta{Key: "rootfs", StoragePath: "/snap/rootfs"})
		case snapshotChain:
			if err := json.NewDecoder(r.Body).Decode(&chainReq); err != nil {
				t.Fatalf("decode chain request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(SnapshotChainResponse{
				Info:       SnapshotMeta{Key: "rootfs", Parent: "parent"},
				ChainPaths: []string{"/snap/parent", "/snap/rootfs"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL)
	importResp, err := c.ImportImageArchive(context.Background(), ImportImageRequest{
		ArchivePath: archive.Name(),
		Namespace:   "team-a",
		ImportedTag: "buildah-oci-rootfs:latest",
	})
	if err != nil {
		t.Fatalf("ImportImageArchive: %v", err)
	}
	if importResp.SnapshotKey != "snapshot-id" || importResp.ImageName != "image:latest" {
		t.Fatalf("import response = %#v", importResp)
	}
	if importedTag != "buildah-oci-rootfs:latest" || importedNamespace != "team-a" || archiveBody != "archive-content" {
		t.Fatalf("multipart values tag=%q namespace=%q body=%q", importedTag, importedNamespace, archiveBody)
	}

	if err := c.LinkRootfsSnapshotToVM(context.Background(), LinkSnapshotVMRequest{
		RootfsSnapshotID: "rootfs-id",
		VMSnapshotID:     "vm-id",
		Namespace:        "team-a",
	}); err != nil {
		t.Fatalf("LinkRootfsSnapshotToVM: %v", err)
	}
	if linkReq.RootfsSnapshotID != "rootfs-id" || linkReq.VMSnapshotID != "vm-id" || linkReq.Namespace != "team-a" {
		t.Fatalf("link request = %#v", linkReq)
	}

	info, err := c.SnapshotInfo(context.Background(), SnapshotInfoRequest{Key: "rootfs", Namespace: "team-a"})
	if err != nil {
		t.Fatalf("SnapshotInfo: %v", err)
	}
	if info.StoragePath != "/snap/rootfs" || infoReq.Key != "rootfs" {
		t.Fatalf("info=%#v req=%#v", info, infoReq)
	}

	chain, err := c.SnapshotChain(context.Background(), SnapshotInfoRequest{Key: "rootfs", Namespace: "team-a"})
	if err != nil {
		t.Fatalf("SnapshotChain: %v", err)
	}
	if len(chain.ChainPaths) != 2 || chain.ChainPaths[1] != "/snap/rootfs" || chainReq.Key != "rootfs" {
		t.Fatalf("chain=%#v req=%#v", chain, chainReq)
	}
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

	_, _, err := ResolveKernelPaths(tmpDir, "missing-vmlinuz", "missing-initrd")
	if err == nil {
		t.Error("expected error for missing files")
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
