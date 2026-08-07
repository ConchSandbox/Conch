package image_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	conchimage "github.com/openeuler/Conch/internal/image"
)

func TestTemplateRemovalKeepsSharedComponentsAndReleasesRuntimeLease(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required")
	}
	host, err := containerdhost.Start(context.Background(), containerdhost.Config{
		RootDir:  t.TempDir(),
		StateDir: t.TempDir(),
		Snapshot: containerdhost.SnapshotConfig{WorkDir: t.TempDir()},
	})
	if err != nil {
		t.Skipf("embedded containerd host unavailable: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })

	ctx := containerdclient.NewNamespaceContext(context.Background())
	buildCtx, done, err := host.Client().WithLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rootfsDesc, err := conchimage.BuildNativeComponentInContent(buildCtx, host.Client().ContentStore(), []string{writeTemplateCaptureRoot(t, "cleanup-rootfs")}, conchimage.KindRootfs, "localhost/conch/rootfs:cleanup")
	if err != nil {
		t.Fatal(err)
	}
	sandboxDesc, err := conchimage.BuildNativeComponentInContent(buildCtx, host.Client().ContentStore(), []string{writeTemplateCaptureRoot(t, "cleanup-sandbox")}, conchimage.KindSandbox, "localhost/conch/sandbox:cleanup")
	if err != nil {
		t.Fatal(err)
	}
	memDesc, err := conchimage.BuildNativeComponentInContent(buildCtx, host.Client().ContentStore(), []string{writeTemplateCaptureRoot(t, "cleanup-memory")}, conchimage.KindMemSnapshot, "localhost/conch/memory:cleanup")
	if err != nil {
		t.Fatal(err)
	}
	coldRef := "localhost/conch/template:cleanup-cold"
	coldIndex, err := conchimage.BuildBootIndexInContent(buildCtx, host.Client().ContentStore(), conchimage.BootIndexContentOptions{
		RootfsDescriptor: rootfsDesc, SandboxDescriptor: sandboxDesc, Tag: coldRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeRef := "localhost/conch/template:cleanup-resume"
	resumeIndex, err := conchimage.BuildBootIndexInContent(buildCtx, host.Client().ContentStore(), conchimage.BootIndexContentOptions{
		RootfsDescriptor: rootfsDesc, MemDescriptor: memDesc, SandboxDescriptor: sandboxDesc,
		Tag: resumeRef, VMMName: "cloud-hypervisor", MemorySizeMB: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]images.Image{
		coldRef:   {Name: coldRef, Target: coldIndex},
		resumeRef: {Name: resumeRef, Target: resumeIndex},
	} {
		if _, err := host.Client().ImageService().Create(buildCtx, target); err != nil {
			t.Fatalf("create image %s: %v", name, err)
		}
	}
	if err := conchimage.RetainTemplateResources(ctx, host.Client(), "cold", coldIndex.Digest.String()); err != nil {
		t.Fatalf("retain cold template: %v", err)
	}
	if err := conchimage.RetainTemplateResources(ctx, host.Client(), "resume", resumeIndex.Digest.String()); err != nil {
		t.Fatalf("retain resume template: %v", err)
	}
	if err := done(buildCtx); err != nil {
		t.Fatalf("release build lease: %v", err)
	}

	runtimeCtx, _, err := host.Client().WithRuntimeLease(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := conchimage.UnpackBootIndex(runtimeCtx, host.Client(), coldIndex.Digest.String()); err != nil {
		t.Fatalf("unpack cold template: %v", err)
	}
	if err := conchimage.UnpackBootIndex(runtimeCtx, host.Client(), resumeIndex.Digest.String()); err != nil {
		t.Fatalf("unpack resume template: %v", err)
	}
	before := templateSnapshotCount(t, ctx, host.Client().SnapshotService("erofs"))
	if before == 0 {
		t.Fatal("unpack created no snapshots")
	}

	coldPlan, err := conchimage.PlanTemplateRemoval(ctx, host.Client(), conchimage.TemplateRemovalOptions{
		Target:   conchimage.TemplateResourceReference{TemplateID: "cold", BootIndexDigest: coldIndex.Digest.String(), BuildRef: coldRef},
		Retained: []conchimage.TemplateResourceReference{{TemplateID: "resume", BootIndexDigest: resumeIndex.Digest.String(), BuildRef: resumeRef}},
	})
	if err != nil {
		t.Fatalf("plan cold removal: %v", err)
	}
	if len(coldPlan.ImageNames) != 1 || coldPlan.ImageNames[0] != coldRef || len(coldPlan.SnapshotKeys) != 0 {
		t.Fatalf("cold removal plan = %#v, want only cold Boot Index", coldPlan)
	}
	if err := conchimage.ApplyTemplateRemoval(ctx, host.Client(), coldPlan); err != nil {
		t.Fatalf("apply cold removal: %v", err)
	}
	if _, err := host.Client().ImageService().Get(ctx, coldRef); !errdefs.IsNotFound(err) {
		t.Fatalf("cold Boot Index still exists or lookup failed: %v", err)
	}
	if got := templateSnapshotCount(t, ctx, host.Client().SnapshotService("erofs")); got != before {
		t.Fatalf("shared snapshots changed from %d to %d", before, got)
	}
	assertTemplateLeaseMissing(t, ctx, host.Client().LeasesService(), templateLeaseIDForTest("cold"))
	assertTemplateLeasePresent(t, ctx, host.Client().LeasesService(), templateLeaseIDForTest("resume"))

	lastPlan, err := conchimage.PlanTemplateRemoval(ctx, host.Client(), conchimage.TemplateRemovalOptions{
		Target: conchimage.TemplateResourceReference{TemplateID: "resume", BootIndexDigest: resumeIndex.Digest.String(), BuildRef: resumeRef},
	})
	if err != nil {
		t.Fatalf("plan last removal: %v", err)
	}
	if len(lastPlan.ImageNames) != 4 || len(lastPlan.SnapshotKeys) == 0 {
		t.Fatalf("last removal plan = %#v, want Boot Index, three components, and snapshots", lastPlan)
	}
	if err := conchimage.ApplyTemplateRemoval(ctx, host.Client(), lastPlan); err != nil {
		t.Fatalf("apply last removal: %v", err)
	}
	if got := templateSnapshotCount(t, ctx, host.Client().SnapshotService("erofs")); got != 0 {
		t.Fatalf("snapshot count after last template removal = %d, want 0", got)
	}
	assertTemplateSnapshotLeaseRefsRemoved(t, ctx, host.Client().LeasesService(), containerdclient.RuntimeLeaseID(), lastPlan.SnapshotKeys)
	assertTemplateLeaseMissing(t, ctx, host.Client().LeasesService(), templateLeaseIDForTest("resume"))
}

func templateLeaseIDForTest(templateID string) string {
	return "conch.template." + templateID
}

func writeTemplateCaptureRoot(t *testing.T, value string) string {
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

func templateSnapshotCount(t *testing.T, ctx context.Context, snapshotter snapshots.Snapshotter) int {
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

func assertTemplateLeasePresent(t *testing.T, ctx context.Context, manager leases.Manager, leaseID string) {
	t.Helper()
	if _, err := manager.ListResources(ctx, leases.Lease{ID: leaseID}); err != nil {
		t.Fatalf("lease %s is not present: %v", leaseID, err)
	}
}

func assertTemplateLeaseMissing(t *testing.T, ctx context.Context, manager leases.Manager, leaseID string) {
	t.Helper()
	if _, err := manager.ListResources(ctx, leases.Lease{ID: leaseID}); !errdefs.IsNotFound(err) {
		t.Fatalf("lease %s lookup error = %v, want not found", leaseID, err)
	}
}

func assertTemplateSnapshotLeaseRefsRemoved(t *testing.T, ctx context.Context, manager leases.Manager, leaseID string, keys []string) {
	t.Helper()
	resources, err := manager.ListResources(ctx, leases.Lease{ID: leaseID})
	if err != nil {
		t.Fatalf("list runtime lease resources: %v", err)
	}
	wantRemoved := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wantRemoved[key] = struct{}{}
	}
	for _, resource := range resources {
		if resource.Type != "snapshots/erofs" {
			continue
		}
		if _, found := wantRemoved[resource.ID]; found {
			t.Fatalf("snapshot %s remains in runtime lease", resource.ID)
		}
	}
}
