package template

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/snapshot"
)

func TestManagerUsesColdPathForTemplateWithoutMem(t *testing.T) {
	ctx := context.Background()
	store := NewStore(newTestStateStore(t))
	rec, err := store.Create(ctx, CreateRequest{Origin: state.TemplateOriginImage, Namespace: "team-a"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.MarkReady(ctx, rec.ID, Refs{RootfsKey: "rootfs", VMKey: "vm"}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	backend := &fakeSnapshotBackend{}
	manager := NewManager(store, backend)

	got, err := manager.PrepareSandboxBoot(ctx, PrepareSandboxBootRequest{
		Namespace:  "team-a",
		TemplateID: rec.ID,
		SandboxID:  "sandbox-a",
		RamMB:      512,
	})
	if err != nil {
		t.Fatalf("PrepareSandboxBoot() error = %v", err)
	}
	if got.Runtime.ParentRootfsID != "rootfs" || got.Runtime.ParentMemID != "" || got.Runtime.ParentVMID != "vm" {
		t.Fatalf("runtime = %#v", got.Runtime)
	}
	if got.Runtime.Resume {
		t.Fatalf("runtime.Resume = true, want false")
	}
	if !slices.Equal(backend.statKeys, []string{"rootfs", "vm"}) || !backend.createCalled || backend.restoreCalled {
		t.Fatalf("backend calls = %#v", backend)
	}
}

func TestManagerUsesStrictPathForResumeTemplate(t *testing.T) {
	ctx := context.Background()
	store := NewStore(newTestStateStore(t))
	rec, err := store.Create(ctx, CreateRequest{Origin: state.TemplateOriginCheckpoint, Namespace: "team-a"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.MarkReady(ctx, rec.ID, Refs{RootfsKey: "rootfs", MemKey: "mem", VMKey: "vm"}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	backend := &fakeSnapshotBackend{}
	manager := NewManager(store, backend)

	got, err := manager.PrepareSandboxBoot(ctx, PrepareSandboxBootRequest{
		Namespace:       "team-a",
		TemplateID:      rec.ID,
		SandboxID:       "sandbox-a",
		VsockCID:        7,
		VsockSocketPath: "/tmp/vsock.sock",
	})
	if err != nil {
		t.Fatalf("PrepareSandboxBoot() error = %v", err)
	}
	if !got.Runtime.Resume {
		t.Fatalf("runtime = %#v, want restorable", got.Runtime)
	}
	if !slices.Equal(backend.statKeys, []string{"rootfs", "mem", "vm"}) || !backend.restoreCalled || backend.createCalled {
		t.Fatalf("backend calls = %#v", backend)
	}
}

func TestManagerRejectsNonReadyTemplate(t *testing.T) {
	ctx := context.Background()
	store := NewStore(newTestStateStore(t))
	rec, err := store.Create(ctx, CreateRequest{Origin: state.TemplateOriginImage})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err = NewManager(store, &fakeSnapshotBackend{}).PrepareSandboxBoot(ctx, PrepareSandboxBootRequest{
		TemplateID: rec.ID,
		SandboxID:  "sandbox-a",
	})
	if err == nil || !strings.Contains(err.Error(), state.TemplateCreating) {
		t.Fatalf("PrepareSandboxBoot() error = %v, want creating error", err)
	}
}

func TestManagerUsesTemplateIdentityForDistinctCommits(t *testing.T) {
	ctx := context.Background()
	backend := &fakeSnapshotBackend{}
	manager := NewManager(nil, backend)

	first, err := manager.CommitSandboxBoot(ctx, CommitSandboxBootRequest{
		Namespace:   "team-a",
		SandboxID:   "sandbox-a",
		TemplateID:  "tmpl_first",
		CapturePath: "/capture/first",
		ParentVMID:  "vm",
	})
	if err != nil {
		t.Fatalf("first CommitSandboxBoot() error = %v", err)
	}
	second, err := manager.CommitSandboxBoot(ctx, CommitSandboxBootRequest{
		Namespace:   "team-a",
		SandboxID:   "sandbox-a",
		TemplateID:  "tmpl_second",
		CapturePath: "/capture/second",
		ParentVMID:  "vm",
	})
	if err != nil {
		t.Fatalf("second CommitSandboxBoot() error = %v", err)
	}
	if first.RootfsKey == second.RootfsKey {
		t.Fatalf("checkpoint rootfs refs are equal: %q", first.RootfsKey)
	}
	if len(backend.commitSnapshotIDs) != 2 || backend.commitSnapshotIDs[0] == backend.commitSnapshotIDs[1] {
		t.Fatalf("committed snapshot ids = %#v, want two distinct ids", backend.commitSnapshotIDs)
	}
	if !slices.Equal(backend.commitCapturePaths, []string{"/capture/first", "/capture/second"}) {
		t.Fatalf("commit capture paths = %#v", backend.commitCapturePaths)
	}
}

type fakeSnapshotBackend struct {
	statKeys           []string
	createCalled       bool
	restoreCalled      bool
	commitSnapshotIDs  []string
	commitCapturePaths []string
}

func (f *fakeSnapshotBackend) CreateBootLayout(ctx context.Context, namespace, key string, parents snapshot.ParentSnapshotIDs, memorySizeMB int64) (*snapshot.BootLayout, error) {
	f.createCalled = true
	return fakeBootLayout(memorySizeMB), nil
}

func (f *fakeSnapshotBackend) RestoreBootLayout(ctx context.Context, namespace, key string, parents snapshot.ParentSnapshotIDs, cid uint32, socketPath string) (*snapshot.BootLayout, error) {
	f.restoreCalled = true
	return fakeBootLayout(256), nil
}

func (f *fakeSnapshotBackend) ReleaseBootLayout(ctx context.Context, namespace, key string) error {
	return nil
}

func (f *fakeSnapshotBackend) SnapshotInfo(ctx context.Context, namespace, key string) (snapshots.Info, error) {
	f.statKeys = append(f.statKeys, key)
	return snapshots.Info{Parent: "parent"}, nil
}

func (f *fakeSnapshotBackend) CommitBootLayout(ctx context.Context, namespace, snapshotID, key, capturePath, parentVMID string) (string, error) {
	f.commitSnapshotIDs = append(f.commitSnapshotIDs, snapshotID)
	f.commitCapturePaths = append(f.commitCapturePaths, capturePath)
	return snapshotID, nil
}

func fakeBootLayout(memorySizeMB int64) *snapshot.BootLayout {
	if memorySizeMB <= 0 {
		memorySizeMB = 256
	}
	return &snapshot.BootLayout{
		RootfsMount:  "/mnt/rootfs",
		MemMount:     "/mnt/mem",
		VMMount:      "/mnt/vm",
		SnapshotDir:  "conch/snapshot",
		MemorySizeMB: memorySizeMB,
	}
}
