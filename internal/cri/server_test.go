package cri

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestRuntimeBasics(t *testing.T) {
	svc := &service{}

	version, err := svc.Version(context.Background(), &runtimev1.VersionRequest{Version: "v1"})
	if err != nil {
		t.Fatalf("Version() error = %v", err)
	}
	if version.GetRuntimeName() != runtimeName || version.GetRuntimeApiVersion() != runtimeAPIVersion {
		t.Fatalf("Version() = %#v", version)
	}

	status, err := svc.Status(context.Background(), &runtimev1.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(status.GetStatus().GetConditions()) != 2 {
		t.Fatalf("Status() conditions = %#v", status.GetStatus().GetConditions())
	}

	cfg, err := svc.RuntimeConfig(context.Background(), &runtimev1.RuntimeConfigRequest{})
	if err != nil {
		t.Fatalf("RuntimeConfig() error = %v", err)
	}
	if cfg.GetLinux().GetCgroupDriver() != runtimev1.CgroupDriver_SYSTEMD {
		t.Fatalf("RuntimeConfig() = %#v", cfg)
	}
}

func TestRemoveStaleUnixSocketRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conch-cri.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := removeStaleUnixSocket(path)
	if err == nil {
		t.Fatalf("removeStaleUnixSocket() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "not a unix socket") {
		t.Fatalf("error = %q, want non-socket error", err.Error())
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("regular file was removed: %v", statErr)
	}
}

func TestImageServiceListStatusAndRemove(t *testing.T) {
	runtime := &fakeRuntime{
		images: []runtimeapi.ImageRecord{
			{
				Name:         "registry.example.invalid/conch/demo:latest",
				TargetDigest: "sha256:demo",
				RepoDigests:  []string{"registry.example.invalid/conch/demo@sha256:demo"},
				Size:         42,
				Kind:         "sandbox-base",
				Labels:       map[string]string{"kind": "conch", "io.conch.kind": "sandbox-base"},
			},
			{
				Name:         "registry.example.invalid/conch/demo:v1",
				TargetDigest: "sha256:demo",
				RepoDigests:  []string{"registry.example.invalid/conch/demo@sha256:demo"},
				Size:         42,
				Kind:         "sandbox-base",
				Labels:       map[string]string{"kind": "conch", "io.conch.kind": "sandbox-base"},
			},
			{
				Name:         "localhost/conch/rootfs-component:component-rootfs",
				TargetDigest: "sha256:component-rootfs",
				RepoDigests:  []string{"localhost/conch/rootfs-component@sha256:component-rootfs"},
				Size:         12,
				Kind:         "rootfs",
				Labels:       map[string]string{"io.conch.kind": "rootfs"},
			},
			{
				Name:         "localhost/conch/sandbox-component:component-sandbox",
				TargetDigest: "sha256:component-sandbox",
				RepoDigests:  []string{"localhost/conch/sandbox-component@sha256:component-sandbox"},
				Size:         8,
				Kind:         "sandbox",
				Labels:       map[string]string{"io.conch.kind": "sandbox"},
			},
			{
				Name:         "conch-erofs-rootfs:convert-123",
				TargetDigest: "sha256:convert-rootfs",
				RepoDigests:  []string{"conch-erofs-rootfs@sha256:convert-rootfs"},
				Size:         4,
			},
			{
				Name:         "conch-kernel:convert-123",
				TargetDigest: "sha256:convert-kernel",
				RepoDigests:  []string{"conch-kernel@sha256:convert-kernel"},
				Size:         4,
			},
			{
				Name:            "docker.io/library/busybox:latest",
				TargetDigest:    "sha256:busybox",
				TargetMediaType: "application/vnd.oci.image.manifest.v1+json",
				RepoDigests:     []string{"docker.io/library/busybox@sha256:busybox"},
				Size:            6,
			},
		},
	}
	svc := &service{runtime: runtime}

	list, err := svc.ListImages(context.Background(), &runtimev1.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages() error = %v", err)
	}
	if len(list.GetImages()) != 1 || list.GetImages()[0].GetId() != "sha256:demo" {
		t.Fatalf("ListImages() = %#v", list.GetImages())
	}
	if got := list.GetImages()[0].GetRepoTags(); len(got) != 2 {
		t.Fatalf("ListImages() repo tags = %#v", got)
	}
	if got := list.GetImages()[0].GetRepoDigests(); len(got) != 1 || got[0] != "registry.example.invalid/conch/demo@sha256:demo" {
		t.Fatalf("ListImages() repo digests = %#v", got)
	}

	status, err := svc.ImageStatus(context.Background(), &runtimev1.ImageStatusRequest{
		Image:   &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/demo:latest"},
		Verbose: true,
	})
	if err != nil {
		t.Fatalf("ImageStatus() error = %v", err)
	}
	if status.GetImage().GetSpec().GetImage() != "registry.example.invalid/conch/demo:latest" {
		t.Fatalf("ImageStatus() = %#v", status.GetImage())
	}
	if status.GetInfo()["conch"] == "" {
		t.Fatalf("ImageStatus() verbose info = %#v", status.GetInfo())
	}
	if !strings.Contains(status.GetInfo()["conch"], `"kind":"sandbox-base"`) {
		t.Fatalf("ImageStatus() verbose kind info = %q", status.GetInfo()["conch"])
	}
	if !strings.Contains(status.GetInfo()["conch"], `"components":["rootfs","sandbox"]`) {
		t.Fatalf("ImageStatus() verbose components info = %q", status.GetInfo()["conch"])
	}
	statusByDigest, err := svc.ImageStatus(context.Background(), &runtimev1.ImageStatusRequest{
		Image: &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/demo@sha256:demo"},
	})
	if err != nil {
		t.Fatalf("ImageStatus(repoDigest) error = %v", err)
	}
	if statusByDigest.GetImage().GetId() != "sha256:demo" {
		t.Fatalf("ImageStatus(repoDigest) = %#v", statusByDigest.GetImage())
	}
	missing, err := svc.ImageStatus(context.Background(), &runtimev1.ImageStatusRequest{
		Image: &runtimev1.ImageSpec{Image: "demo"},
	})
	if err != nil {
		t.Fatalf("ImageStatus() substring error = %v", err)
	}
	if missing.GetImage() != nil {
		t.Fatalf("ImageStatus() substring match = %#v, want nil", missing.GetImage())
	}
	internal, err := svc.ImageStatus(context.Background(), &runtimev1.ImageStatusRequest{
		Image: &runtimev1.ImageSpec{Image: "conch-erofs-rootfs:convert-123"},
	})
	if err != nil {
		t.Fatalf("ImageStatus() internal convert error = %v", err)
	}
	if internal.GetImage() != nil {
		t.Fatalf("ImageStatus() internal convert image = %#v, want nil", internal.GetImage())
	}
	unknown, err := svc.ImageStatus(context.Background(), &runtimev1.ImageStatusRequest{
		Image: &runtimev1.ImageSpec{Image: "docker.io/library/busybox:latest"},
	})
	if err != nil {
		t.Fatalf("ImageStatus() unknown kind error = %v", err)
	}
	if unknown.GetImage() != nil {
		t.Fatalf("ImageStatus() unknown kind image = %#v, want nil", unknown.GetImage())
	}

	_, err = svc.RemoveImage(context.Background(), &runtimev1.RemoveImageRequest{
		Image: &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/demo:latest"},
	})
	if err != nil {
		t.Fatalf("RemoveImage() error = %v", err)
	}
	if len(runtime.removeImageReqs) != 2 {
		t.Fatalf("RemoveImage requests = %#v", runtime.removeImageReqs)
	}
	if runtime.removeImageReqs[0].ImageName != "registry.example.invalid/conch/demo:latest" || !runtime.removeImageReqs[0].Synchronous {
		t.Fatalf("RemoveImage request[0] = %#v", runtime.removeImageReqs[0])
	}
	if runtime.removeImageReqs[1].ImageName != "registry.example.invalid/conch/demo:v1" || !runtime.removeImageReqs[1].Synchronous {
		t.Fatalf("RemoveImage request[1] = %#v", runtime.removeImageReqs[1])
	}

	runtime.removeImageReqs = nil
	runtime.removeImageErrs = map[string]error{
		"registry.example.invalid/conch/demo:latest": errdefs.ErrNotFound,
		"registry.example.invalid/conch/demo:v1":     errdefs.ErrNotFound,
	}
	_, err = svc.RemoveImage(context.Background(), &runtimev1.RemoveImageRequest{
		Image: &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/demo:latest"},
	})
	if err != nil {
		t.Fatalf("RemoveImage() idempotent error = %v", err)
	}
	if len(runtime.removeImageReqs) != 2 {
		t.Fatalf("RemoveImage idempotent requests = %#v", runtime.removeImageReqs)
	}

	runtime.removeImageReqs = nil
	_, err = svc.RemoveImage(context.Background(), &runtimev1.RemoveImageRequest{
		Image: &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/missing:latest"},
	})
	if err != nil {
		t.Fatalf("RemoveImage() missing image error = %v", err)
	}
	if len(runtime.removeImageReqs) != 0 {
		t.Fatalf("RemoveImage missing image requests = %#v, want none", runtime.removeImageReqs)
	}
	_, err = svc.RemoveImage(context.Background(), &runtimev1.RemoveImageRequest{
		Image: &runtimev1.ImageSpec{Image: "conch-kernel:convert-123"},
	})
	if err != nil {
		t.Fatalf("RemoveImage() internal convert image error = %v", err)
	}
	if len(runtime.removeImageReqs) != 0 {
		t.Fatalf("RemoveImage internal convert requests = %#v, want none", runtime.removeImageReqs)
	}

	fsInfo, err := svc.ImageFsInfo(context.Background(), &runtimev1.ImageFsInfoRequest{})
	if err != nil {
		t.Fatalf("ImageFsInfo() error = %v", err)
	}
	if fsInfo == nil {
		t.Fatal("ImageFsInfo() response = nil")
	}
}

func TestImageServiceGRPCListStatusAndRemove(t *testing.T) {
	runtime := &fakeRuntime{
		images: []runtimeapi.ImageRecord{
			{
				Name:         "registry.example.invalid/conch/grpc-demo:latest",
				TargetDigest: "sha256:grpcdemo",
				RepoDigests:  []string{"registry.example.invalid/conch/grpc-demo@sha256:grpcdemo"},
				Size:         128,
				Kind:         "sandbox-base",
				Labels:       map[string]string{"kind": "conch", "io.conch.kind": "sandbox-base"},
			},
			{
				Name:         "registry.example.invalid/conch/grpc-demo:v1",
				TargetDigest: "sha256:grpcdemo",
				RepoDigests:  []string{"registry.example.invalid/conch/grpc-demo@sha256:grpcdemo"},
				Size:         128,
				Kind:         "sandbox-base",
				Labels:       map[string]string{"kind": "conch", "io.conch.kind": "sandbox-base"},
			},
			{
				Name:         "localhost/conch/rootfs-component:grpc-component-rootfs",
				TargetDigest: "sha256:grpc-component-rootfs",
				RepoDigests:  []string{"localhost/conch/rootfs-component@sha256:grpc-component-rootfs"},
				Size:         24,
				Kind:         "rootfs",
				Labels:       map[string]string{"io.conch.kind": "rootfs"},
			},
			{
				Name:         "localhost/conch/sandbox-component:grpc-component-sandbox",
				TargetDigest: "sha256:grpc-component-sandbox",
				RepoDigests:  []string{"localhost/conch/sandbox-component@sha256:grpc-component-sandbox"},
				Size:         16,
				Kind:         "sandbox",
				Labels:       map[string]string{"io.conch.kind": "sandbox"},
			},
		},
	}
	socketPath := filepath.Join(t.TempDir(), "conch-cri.sock")
	server := New(Config{Socket: socketPath}, runtime, nil, nil)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := runtimev1.NewImageServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	list, err := client.ListImages(ctx, &runtimev1.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages() error = %v", err)
	}
	if len(list.GetImages()) != 1 || list.GetImages()[0].GetId() != "sha256:grpcdemo" {
		t.Fatalf("ListImages() = %#v", list.GetImages())
	}
	if got := list.GetImages()[0].GetRepoTags(); len(got) != 2 {
		t.Fatalf("ListImages() repo tags = %#v", got)
	}
	if got := list.GetImages()[0].GetRepoDigests(); len(got) != 1 || got[0] != "registry.example.invalid/conch/grpc-demo@sha256:grpcdemo" {
		t.Fatalf("ListImages() repo digests = %#v", got)
	}

	status, err := client.ImageStatus(ctx, &runtimev1.ImageStatusRequest{
		Image:   &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/grpc-demo:latest"},
		Verbose: true,
	})
	if err != nil {
		t.Fatalf("ImageStatus() error = %v", err)
	}
	if status.GetImage().GetId() != "sha256:grpcdemo" {
		t.Fatalf("ImageStatus() = %#v", status.GetImage())
	}
	if status.GetInfo()["conch"] == "" {
		t.Fatalf("ImageStatus() verbose info = %#v", status.GetInfo())
	}
	if !strings.Contains(status.GetInfo()["conch"], `"kind":"sandbox-base"`) {
		t.Fatalf("ImageStatus() verbose kind info = %q", status.GetInfo()["conch"])
	}
	if !strings.Contains(status.GetInfo()["conch"], `"components":["rootfs","sandbox"]`) {
		t.Fatalf("ImageStatus() verbose components info = %q", status.GetInfo()["conch"])
	}
	statusByDigest, err := client.ImageStatus(ctx, &runtimev1.ImageStatusRequest{
		Image: &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/grpc-demo@sha256:grpcdemo"},
	})
	if err != nil {
		t.Fatalf("ImageStatus(repoDigest) error = %v", err)
	}
	if statusByDigest.GetImage().GetId() != "sha256:grpcdemo" {
		t.Fatalf("ImageStatus(repoDigest) = %#v", statusByDigest.GetImage())
	}

	_, err = client.RemoveImage(ctx, &runtimev1.RemoveImageRequest{
		Image: &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/grpc-demo:latest"},
	})
	if err != nil {
		t.Fatalf("RemoveImage() error = %v", err)
	}
	if len(runtime.removeImageReqs) != 2 {
		t.Fatalf("RemoveImage requests = %#v", runtime.removeImageReqs)
	}
	if runtime.removeImageReqs[0].ImageName != "registry.example.invalid/conch/grpc-demo:latest" || !runtime.removeImageReqs[0].Synchronous {
		t.Fatalf("RemoveImage request[0] = %#v", runtime.removeImageReqs[0])
	}
	if runtime.removeImageReqs[1].ImageName != "registry.example.invalid/conch/grpc-demo:v1" || !runtime.removeImageReqs[1].Synchronous {
		t.Fatalf("RemoveImage request[1] = %#v", runtime.removeImageReqs[1])
	}

	runtime.removeImageReqs = nil
	runtime.removeImageErrs = map[string]error{
		"registry.example.invalid/conch/grpc-demo:latest": errdefs.ErrNotFound,
		"registry.example.invalid/conch/grpc-demo:v1":     errdefs.ErrNotFound,
	}
	_, err = client.RemoveImage(ctx, &runtimev1.RemoveImageRequest{
		Image: &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/grpc-demo:latest"},
	})
	if err != nil {
		t.Fatalf("RemoveImage() idempotent error = %v", err)
	}
	if len(runtime.removeImageReqs) != 2 {
		t.Fatalf("RemoveImage idempotent requests = %#v", runtime.removeImageReqs)
	}

	fsInfo, err := client.ImageFsInfo(ctx, &runtimev1.ImageFsInfoRequest{})
	if err != nil {
		t.Fatalf("ImageFsInfo() error = %v", err)
	}
	if fsInfo == nil {
		t.Fatal("ImageFsInfo() response = nil")
	}
}

func TestPullImageResolvesToIndexedImageID(t *testing.T) {
	runtime := &fakeRuntime{
		pullResult: runtimeapi.PullImageResult{Refs: map[string]string{"rootfs": "rootfs-snapshot-id"}},
		images: []runtimeapi.ImageRecord{{
			Name:         "registry.example.invalid/conch/pull-demo:latest",
			TargetDigest: "sha256:pulldemo",
			RepoDigests:  []string{"registry.example.invalid/conch/pull-demo@sha256:pulldemo"},
			Kind:         "sandbox-snapshot",
		}},
	}
	svc := &service{
		cfg:     Config{DefaultKernelImage: "default-kernel"},
		runtime: runtime,
	}

	resp, err := svc.PullImage(context.Background(), &runtimev1.PullImageRequest{
		Image: &runtimev1.ImageSpec{Image: "registry.example.invalid/conch/pull-demo:latest"},
		Auth:  &runtimev1.AuthConfig{Username: "user", Password: "pass"},
		SandboxConfig: &runtimev1.PodSandboxConfig{
			Metadata: &runtimev1.PodSandboxMetadata{Namespace: "team-a"},
		},
	})
	if err != nil {
		t.Fatalf("PullImage() error = %v", err)
	}
	if resp.GetImageRef() != "sha256:pulldemo" {
		t.Fatalf("PullImage() image ref = %q", resp.GetImageRef())
	}
	if runtime.pullImageReq.ImageName != "registry.example.invalid/conch/pull-demo:latest" ||
		runtime.pullImageReq.Namespace != "" ||
		runtime.pullImageReq.Username != "user" ||
		runtime.pullImageReq.Password != "pass" ||
		runtime.pullImageReq.DefaultKernelImage != "default-kernel" {
		t.Fatalf("PullImage request = %#v", runtime.pullImageReq)
	}
	if runtime.listImagesReq.Namespace != "" {
		t.Fatalf("PullImage ListImages namespace = %q, want fixed default namespace", runtime.listImagesReq.Namespace)
	}
}
