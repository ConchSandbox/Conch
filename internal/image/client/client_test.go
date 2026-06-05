package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	var listReq ListImagesRequest
	var removeReq RemoveImageRequest
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
		case listImages:
			if err := json.NewDecoder(r.Body).Decode(&listReq); err != nil {
				t.Fatalf("decode list request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(ListImagesResponse{Images: []ImageRecord{{
				Name:         "localhost/conch/demo:latest",
				TargetDigest: "sha256:demo",
				Size:         42,
				Kind:         "sandbox-base",
			}}})
		case removeImage:
			if err := json.NewDecoder(r.Body).Decode(&removeReq); err != nil {
				t.Fatalf("decode remove request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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

	images, err := c.ListImages(context.Background(), ListImagesRequest{
		Namespace: "team-a",
		Filters:   []string{"name==localhost/conch/demo:latest"},
	})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if listReq.Namespace != "team-a" || len(listReq.Filters) != 1 {
		t.Fatalf("list request = %#v", listReq)
	}
	if len(images) != 1 || images[0].Name != "localhost/conch/demo:latest" {
		t.Fatalf("images = %#v", images)
	}
	if images[0].Kind != "sandbox-base" {
		t.Fatalf("image kind = %q, want sandbox-base", images[0].Kind)
	}

	if err := c.RemoveImage(context.Background(), RemoveImageRequest{
		ImageName:   "localhost/conch/demo:latest",
		Namespace:   "team-a",
		Synchronous: true,
	}); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if removeReq.ImageName != "localhost/conch/demo:latest" || removeReq.Namespace != "team-a" || !removeReq.Synchronous {
		t.Fatalf("remove request = %#v", removeReq)
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

func TestConchAPITimeoutOverride(t *testing.T) {
	t.Setenv("CONCH_API_TIMEOUT", "5m")
	c := NewClientWithConfigAndTimeout("http://127.0.0.1:4063", "", 10*time.Minute)
	if c.httpClient.Timeout != 10*time.Minute {
		t.Fatalf("timeout = %s, want 10m", c.httpClient.Timeout)
	}
}

func TestConvertAndSnapshotExportAPIMethods(t *testing.T) {
	kernel, err := os.CreateTemp(t.TempDir(), "kernel-*")
	if err != nil {
		t.Fatalf("CreateTemp kernel: %v", err)
	}
	if _, err := kernel.WriteString("kernel-content"); err != nil {
		t.Fatalf("write kernel: %v", err)
	}
	if err := kernel.Close(); err != nil {
		t.Fatalf("close kernel: %v", err)
	}
	initrd, err := os.CreateTemp(t.TempDir(), "initrd-*")
	if err != nil {
		t.Fatalf("CreateTemp initrd: %v", err)
	}
	if _, err := initrd.WriteString("initrd-content"); err != nil {
		t.Fatalf("write initrd: %v", err)
	}
	if err := initrd.Close(); err != nil {
		t.Fatalf("close initrd: %v", err)
	}

	var metadata ConvertImageMetadata
	var snapshotReq SnapshotExportRequest
	var listSnapshotsReq ListSnapshotsRequest
	var removeSnapshotReq RemoveSnapshotRequest
	var kernelBody string
	var initrdBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case convertImage:
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
			_ = json.NewEncoder(w).Encode(ConvertImageResponse{
				BootIndexDigest: "sha256:boot",
				BootIndexTag:    "localhost/conch/demo:latest",
				RootfsImageRef:  "conch-erofs-rootfs:build-123",
				SourceImageRef:  "docker.io/library/nginx:latest",
			})
		case snapshotExport:
			if err := json.NewDecoder(r.Body).Decode(&snapshotReq); err != nil {
				t.Fatalf("decode snapshot export request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(SnapshotExportResponse{
				BootIndexDigest: "sha256:snapshot",
				BootIndexTag:    snapshotReq.BootIndexTag,
			})
		case listSnapshots:
			if err := json.NewDecoder(r.Body).Decode(&listSnapshotsReq); err != nil {
				t.Fatalf("decode snapshot list request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(ListSnapshotsResponse{Snapshots: []SnapshotRecord{{
				Key:    "sha256:rootfs",
				Kind:   "committed",
				Parent: "sha256:parent",
			}}})
		case removeSnapshot:
			if err := json.NewDecoder(r.Body).Decode(&removeSnapshotReq); err != nil {
				t.Fatalf("decode snapshot remove request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL)
	convertResp, err := c.ConvertImage(context.Background(), ConvertImageRequest{
		Source:       "docker.io/library/nginx:latest",
		KernelPath:   kernel.Name(),
		InitrdPath:   initrd.Name(),
		BootIndexTag: "localhost/conch/demo:latest",
		Namespace:    "team-a",
		PlainHTTP:    true,
		Username:     "user",
		Password:     "pass",
		Snapshot:     true,
	})
	if err != nil {
		t.Fatalf("ConvertImage: %v", err)
	}
	if metadata.Source != "docker.io/library/nginx:latest" || metadata.Namespace != "team-a" || metadata.BootIndexTag != "localhost/conch/demo:latest" || !metadata.Snapshot || !metadata.PlainHTTP {
		t.Fatalf("metadata = %#v", metadata)
	}
	if kernelBody != "kernel-content" || initrdBody != "initrd-content" {
		t.Fatalf("uploaded bodies kernel=%q initrd=%q", kernelBody, initrdBody)
	}
	if convertResp.BootIndexDigest != "sha256:boot" || convertResp.RootfsImageRef != "conch-erofs-rootfs:build-123" {
		t.Fatalf("convert response = %#v", convertResp)
	}

	snapshotResp, err := c.ExportSnapshot(context.Background(), SnapshotExportRequest{
		Namespace:        "team-a",
		BootIndexTag:     "localhost/conch/snap:latest",
		RootfsSnapshotID: "rootfs-id",
	})
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if snapshotReq.RootfsSnapshotID != "rootfs-id" || snapshotReq.Namespace != "team-a" || snapshotResp.BootIndexDigest != "sha256:snapshot" {
		t.Fatalf("snapshot req=%#v resp=%#v", snapshotReq, snapshotResp)
	}

	snapshots, err := c.ListSnapshots(context.Background(), ListSnapshotsRequest{
		Namespace: "team-a",
		Filters:   []string{"kind==committed"},
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if listSnapshotsReq.Namespace != "team-a" || len(listSnapshotsReq.Filters) != 1 {
		t.Fatalf("snapshot list request = %#v", listSnapshotsReq)
	}
	if len(snapshots) != 1 || snapshots[0].Key != "sha256:rootfs" {
		t.Fatalf("snapshots = %#v", snapshots)
	}

	if err := c.RemoveSnapshot(context.Background(), RemoveSnapshotRequest{
		Key:       "sha256:rootfs",
		Namespace: "team-a",
		Cascade:   true,
	}); err != nil {
		t.Fatalf("RemoveSnapshot: %v", err)
	}
	if removeSnapshotReq.Key != "sha256:rootfs" || removeSnapshotReq.Namespace != "team-a" || !removeSnapshotReq.Cascade {
		t.Fatalf("snapshot remove request = %#v", removeSnapshotReq)
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

func TestCreateSandboxIncludesNamespace(t *testing.T) {
	var got CreateRequest
	c := NewClient("http://example.invalid")
	c.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != createSandbox {
				t.Fatalf("path = %q, want %q", r.URL.Path, createSandbox)
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(`{"status":"ok","ip":"192.0.2.2"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	if err := c.CreateSandbox("rootfs:latest", "sandbox-123", "team-a", DefaultRamMB); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if got.ImageName != "rootfs:latest" || got.SandboxId != "sandbox-123" {
		t.Fatalf("create request = %#v", got)
	}
	if got.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want %q", got.Namespace, "team-a")
	}
}
