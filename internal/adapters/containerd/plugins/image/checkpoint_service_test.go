package image_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	conchimage "github.com/openeuler/Conch/internal/image"
)

func TestPublishBootImagePublishesContentWithoutUnpackingSnapshots(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required")
	}
	host, err := containerdhost.Start(context.Background(), containerdhost.Config{
		RootDir:          t.TempDir(),
		StateDir:         t.TempDir(),
		DefaultNamespace: "publish-test",
		Snapshot:         containerdhost.SnapshotConfig{WorkDir: t.TempDir()},
	})
	if err != nil {
		t.Skipf("embedded containerd host unavailable: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	ctx := namespaces.WithNamespace(context.Background(), "publish-test")
	leaseCtx, done, err := host.Client().WithLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { done(leaseCtx) })
	ctx = leaseCtx
	rootfsDesc, err := conchimage.BuildNativeComponentInContent(ctx, host.Client().ContentStore(), []string{writeCaptureRoot(t, "rootfs")}, conchimage.KindRootfs, "localhost/conch/rootfs:publish-test")
	if err != nil {
		t.Fatal(err)
	}
	rootfsName := "localhost/conch/rootfs:publish-test"
	if _, err := host.Client().ImageService().Create(ctx, images.Image{Name: rootfsName, Target: rootfsDesc}); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(t.TempDir(), "vmlinuz")
	initrd := filepath.Join(t.TempDir(), "initrd")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrd, []byte("initrd"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotter := host.Client().SnapshotService("erofs")
	before := snapshotCount(t, ctx, snapshotter)
	result, err := host.ImageService().PublishBootImage(ctx, conchimage.PublishBootImageOptions{
		Namespace:       "publish-test",
		RootfsImageName: rootfsName,
		KernelPath:      kernel,
		InitrdPath:      initrd,
		BootIndexTag:    "localhost/conch/template:publish-test",
	})
	if err != nil {
		t.Fatalf("PublishBootImage() error = %v", err)
	}
	if result.BootIndexDigest == "" || result.ImageName != "localhost/conch/template:publish-test" {
		t.Fatalf("PublishBootImage() = %#v", result)
	}
	if got := snapshotCount(t, ctx, snapshotter); got != before {
		t.Fatalf("PublishBootImage() created snapshots: count changed from %d to %d", before, got)
	}
}

func TestPublishCheckpointBootImageReusesImmutableComponentsAndCreatesNoSnapshots(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required")
	}

	host, err := containerdhost.Start(context.Background(), containerdhost.Config{
		RootDir:          t.TempDir(),
		StateDir:         t.TempDir(),
		DefaultNamespace: "checkpoint-test",
		Snapshot: containerdhost.SnapshotConfig{
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		t.Skipf("embedded containerd host unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Errorf("close host: %v", err)
		}
	})

	ctx := namespaces.WithNamespace(context.Background(), "checkpoint-test")
	leaseCtx, done, err := host.Client().WithLease(ctx)
	if err != nil {
		t.Fatalf("create content lease: %v", err)
	}
	t.Cleanup(func() { done(leaseCtx) })
	ctx = leaseCtx
	store := host.Client().ContentStore()
	rootfsSource := writeCaptureRoot(t, "source-rootfs")
	sandboxSource := writeCaptureRoot(t, "source-sandbox")
	rootfsDesc, err := conchimage.BuildNativeComponentInContent(ctx, store, []string{rootfsSource}, conchimage.KindRootfs, "localhost/conch/source:rootfs")
	if err != nil {
		t.Fatalf("build source rootfs: %v", err)
	}
	sandboxDesc, err := conchimage.BuildNativeComponentInContent(ctx, store, []string{sandboxSource}, conchimage.KindSandbox, "localhost/conch/source:sandbox")
	if err != nil {
		t.Fatalf("build source sandbox: %v", err)
	}
	sourceIndex, err := conchimage.BuildBootIndexInContent(ctx, store, conchimage.BootIndexContentOptions{
		RootfsDescriptor:  rootfsDesc,
		SandboxDescriptor: sandboxDesc,
		Tag:               "localhost/conch/source:latest",
	})
	if err != nil {
		t.Fatalf("build source boot index: %v", err)
	}

	before := snapshotCount(t, ctx, host.Client().SnapshotService("erofs"))
	result, err := host.ImageService().PublishCheckpointBootImage(ctx, conchimage.PublishCheckpointBootImageOptions{
		Namespace:             "checkpoint-test",
		SourceBootIndexDigest: sourceIndex.Digest.String(),
		BootIndexTag:          "localhost/conch/checkpoint:one",
		MemRoot:               writeCaptureRoot(t, "captured-memory"),
		VMMName:               "cloud-hypervisor",
		MemorySizeMB:          512,
	})
	if err != nil {
		t.Fatalf("PublishCheckpointBootImage: %v", err)
	}
	if result.BootIndexDigest == "" || result.ImageName != "localhost/conch/checkpoint:one" {
		t.Fatalf("publish result = %#v", result)
	}
	after := snapshotCount(t, ctx, host.Client().SnapshotService("erofs"))
	if after != before {
		t.Fatalf("snapshot count changed from %d to %d during checkpoint publication", before, after)
	}

	info, err := host.ImageService().InspectBootIndex(ctx, "checkpoint-test", result.BootIndexDigest)
	if err != nil {
		t.Fatalf("InspectBootIndex: %v", err)
	}
	if !info.Resume || info.VMMName != "cloud-hypervisor" || info.MemorySizeMB != 512 {
		t.Fatalf("inspection = %#v", info)
	}
	if info.RootfsDescriptor.Digest != rootfsDesc.Digest {
		t.Fatalf("rootfs digest = %s, want reused %s", info.RootfsDescriptor.Digest, rootfsDesc.Digest)
	}
	if info.SandboxDescriptor.Digest != sandboxDesc.Digest {
		t.Fatalf("sandbox digest = %s, want reused %s", info.SandboxDescriptor.Digest, sandboxDesc.Digest)
	}

	imageRecord, err := host.Client().ImageService().Get(ctx, result.ImageName)
	if err != nil {
		t.Fatalf("get checkpoint image record: %v", err)
	}
	if imageRecord.Target.Digest.String() != result.BootIndexDigest {
		t.Fatalf("tag target = %s, want %s", imageRecord.Target.Digest, result.BootIndexDigest)
	}
	referenceInfo, err := host.ImageService().InspectBootIndexReference(ctx, "checkpoint-test", result.ImageName)
	if err != nil {
		t.Fatalf("InspectBootIndexReference: %v", err)
	}
	if referenceInfo.BootIndexDigest != result.BootIndexDigest || !referenceInfo.Resume || referenceInfo.VMMName != "cloud-hypervisor" || referenceInfo.MemorySizeMB != 512 {
		t.Fatalf("reference inspection = %#v", referenceInfo)
	}
	if got := snapshotCount(t, ctx, host.Client().SnapshotService("erofs")); got != after {
		t.Fatalf("reference inspection created snapshots: count changed from %d to %d", after, got)
	}
	raw, err := content.ReadBlob(ctx, store, imageRecord.Target)
	if err != nil {
		t.Fatalf("read checkpoint index: %v", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("unmarshal checkpoint index: %v", err)
	}
	wantKinds := []string{conchimage.KindRootfs, conchimage.KindMemSnapshot, conchimage.KindSandbox}
	if len(index.Manifests) != len(wantKinds) {
		t.Fatalf("component count = %d, want %d", len(index.Manifests), len(wantKinds))
	}
	for i, want := range wantKinds {
		if got := index.Manifests[i].Annotations["io.conch.kind"]; got != want {
			t.Fatalf("component %d kind = %q, want %q", i, got, want)
		}
	}
}

func writeCaptureRoot(t *testing.T, value string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), value)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func snapshotCount(t *testing.T, ctx context.Context, snapshotter snapshots.Snapshotter) int {
	t.Helper()
	count := 0
	if err := snapshotter.Walk(ctx, func(context.Context, snapshots.Info) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("walk snapshots: %v", err)
	}
	return count
}
