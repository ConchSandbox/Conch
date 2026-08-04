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
				Kind:         "boot-index-cold",
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
		ImageName:  "docker.io/library/nginx:latest",
		Namespace:  "team-a",
		SkipUnpack: true,
	})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if pullReq.ImageName != "docker.io/library/nginx:latest" || pullReq.Namespace != "team-a" || !pullReq.SkipUnpack {
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
	if images[0].Kind != "boot-index-cold" {
		t.Fatalf("image kind = %q, want boot-index-cold", images[0].Kind)
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

func TestTemplateAndSnapshotDebugAPIMethods(t *testing.T) {
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

	var templateMetadata TemplateCreateMetadata
	var listSnapshotsReq ListSnapshotsRequest
	var removeSnapshotReq RemoveSnapshotRequest
	var templateKernelBody string
	var templateInitrdBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case createTemplate:
			if err := r.ParseMultipartForm(4096); err != nil {
				t.Fatalf("ParseMultipartForm: %v", err)
			}
			if err := json.Unmarshal([]byte(r.FormValue("metadata")), &templateMetadata); err != nil {
				t.Fatalf("decode template metadata: %v", err)
			}
			file, _, err := r.FormFile("kernel")
			if err != nil {
				t.Fatalf("template kernel FormFile: %v", err)
			}
			raw, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil {
				t.Fatalf("ReadAll template kernel: %v", err)
			}
			templateKernelBody = string(raw)
			file, _, err = r.FormFile("initrd")
			if err != nil {
				t.Fatalf("template initrd FormFile: %v", err)
			}
			raw, err = io.ReadAll(file)
			_ = file.Close()
			if err != nil {
				t.Fatalf("ReadAll template initrd: %v", err)
			}
			templateInitrdBody = string(raw)
			_ = json.NewEncoder(w).Encode(TemplateCreateResponse{
				Status:          "ok",
				TemplateID:      "tmpl_123",
				BootIndexDigest: "sha256:template",
				BootIndexTag:    "localhost/conch/template:latest",
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
	templateResp, err := c.CreateTemplate(context.Background(), TemplateCreateRequest{
		Source:       "docker.io/library/busybox:latest",
		KernelPath:   kernel.Name(),
		InitrdPath:   initrd.Name(),
		BootIndexTag: "localhost/conch/template:latest",
		Namespace:    "team-a",
		PlainHTTP:    true,
		Username:     "user",
		Password:     "pass",
		Labels:       map[string]string{"role": "base"},
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if templateMetadata.Source != "docker.io/library/busybox:latest" || templateMetadata.Namespace != "team-a" || templateMetadata.BootIndexTag != "localhost/conch/template:latest" || !templateMetadata.PlainHTTP || templateMetadata.Labels["role"] != "base" {
		t.Fatalf("template metadata = %#v", templateMetadata)
	}
	if templateKernelBody != "kernel-content" || templateInitrdBody != "initrd-content" {
		t.Fatalf("uploaded template bodies kernel=%q initrd=%q", templateKernelBody, templateInitrdBody)
	}
	if templateResp.TemplateID != "tmpl_123" || templateResp.BootIndexDigest != "sha256:template" {
		t.Fatalf("template response = %#v", templateResp)
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
	}); err != nil {
		t.Fatalf("RemoveSnapshot: %v", err)
	}
	if removeSnapshotReq.Key != "sha256:rootfs" || removeSnapshotReq.Namespace != "team-a" {
		t.Fatalf("snapshot remove request = %#v", removeSnapshotReq)
	}
}

func TestCheckpointSandboxIncludesNamespace(t *testing.T) {
	var got SandboxCheckpointRequest
	c := NewClient("http://example.invalid")
	c.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != checkpointSandbox {
				t.Fatalf("path = %q, want %q", r.URL.Path, checkpointSandbox)
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(`{"status":"ok","template_id":"tmpl_test"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	templateID, err := c.CheckpointSandbox(context.Background(), "sandbox-123", "team-a")
	if err != nil {
		t.Fatalf("CheckpointSandbox: %v", err)
	}
	if templateID != "tmpl_test" {
		t.Fatalf("templateID = %q, want %q", templateID, "tmpl_test")
	}
	if got.SandboxId != "sandbox-123" {
		t.Fatalf("sandbox_id = %q, want %q", got.SandboxId, "sandbox-123")
	}
	if got.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want %q", got.Namespace, "team-a")
	}
}

func TestTemplateRecordIncludesBootIndexDigestInJSON(t *testing.T) {
	const payload = `{"id":"tmpl_test","boot_index_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	var record TemplateRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if record.BootIndexDigest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("BootIndexDigest = %q", record.BootIndexDigest)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(raw), `"boot_index_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`) {
		t.Fatalf("TemplateRecord JSON = %s", raw)
	}
	for _, removedField := range []string{`"state"`, `"updated_at"`, `"last_error"`} {
		if strings.Contains(string(raw), removedField) {
			t.Fatalf("TemplateRecord JSON still contains removed field %s: %s", removedField, raw)
		}
	}
}

func TestTemplateDistributionAPIMethods(t *testing.T) {
	var pullReq TemplatePullRequest
	var pushReq TemplatePushRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pullTemplate:
			if err := json.NewDecoder(r.Body).Decode(&pullReq); err != nil {
				t.Fatalf("decode pull request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(TemplatePullResponse{
				Status:          "ok",
				TemplateID:      "tmpl_pulled",
				BootIndexDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				BuildRef:        pullReq.Reference,
			})
		case pushTemplate:
			if err := json.NewDecoder(r.Body).Decode(&pushReq); err != nil {
				t.Fatalf("decode push request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c := NewClient(server.URL)

	pulled, err := c.PullTemplate(context.Background(), TemplatePullRequest{
		Reference: "registry.example.invalid/conch/template:latest",
		Namespace: "team-a",
		PlainHTTP: true,
		Username:  "pull-user",
		Password:  "pull-pass",
		Labels:    map[string]string{"source": "registry"},
	})
	if err != nil {
		t.Fatalf("PullTemplate() error = %v", err)
	}
	if pulled.TemplateID != "tmpl_pulled" || pulled.BuildRef != pullReq.Reference || !pullReq.PlainHTTP || pullReq.Namespace != "team-a" {
		t.Fatalf("PullTemplate() response = %#v, request = %#v", pulled, pullReq)
	}

	if err := c.PushTemplate(context.Background(), TemplatePushRequest{
		TemplateID:      pulled.TemplateID,
		RemoteReference: "mirror.example.invalid/conch/template:copy",
		Namespace:       "team-a",
		PlainHTTP:       true,
		Username:        "push-user",
		Password:        "push-pass",
		RegistryTimeout: "10m",
	}); err != nil {
		t.Fatalf("PushTemplate() error = %v", err)
	}
	if pushReq.TemplateID != "tmpl_pulled" || pushReq.RemoteReference != "mirror.example.invalid/conch/template:copy" ||
		pushReq.Namespace != "team-a" || !pushReq.PlainHTTP || pushReq.RegistryTimeout != "10m" {
		t.Fatalf("PushTemplate() request = %#v", pushReq)
	}
}

func TestCreateSandboxIncludesNamespaceAndExplicitRAM(t *testing.T) {
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
				Body:       io.NopCloser(bytes.NewBufferString(`{"sandboxID":"sandbox-123","domain":"192.0.2.2"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	if err := c.CreateSandbox("tmpl_123", "sandbox-123", "team-a", 4096); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if got.TemplateID != "tmpl_123" || got.SandboxId != "sandbox-123" {
		t.Fatalf("create request = %#v", got)
	}
	if got.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want %q", got.Namespace, "team-a")
	}
	if got.RamMB != 4096 {
		t.Fatalf("ram_mb = %d, want 4096", got.RamMB)
	}
	if got.VmmName != "" || got.VcpuNum != 0 {
		t.Fatalf("unexpected client resource defaults = %#v", got)
	}
}
