package conchruntime

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/sandbox"
	runtimeSnapshot "github.com/openeuler/Conch/internal/snapshot"
)

type fakeSandboxOps struct {
	req       sandbox.SandboxCreateRequest
	deleteErr error
}

func (f *fakeSandboxOps) Create(req sandbox.SandboxCreateRequest) (sandbox.SandboxCreateResult, error) {
	f.req = req
	return sandbox.SandboxCreateResult{
		Namespace:  req.Namespace,
		SandboxID:  req.SandboxId,
		IP:         "192.0.2.10",
		AgentToken: req.AgentToken,
	}, nil
}

func (f *fakeSandboxOps) Delete(sandbox.SandboxDeleteRequest) error {
	return f.deleteErr
}

func (f *fakeSandboxOps) Pause(sandbox.SandboxPauseRequest) (string, error) {
	return "", nil
}

func TestRemoveSandboxKeepsStateWhenCleanupFails(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "default",
		State:          state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("cleanup failed")}
	svc := New(sandboxOps, nil, store, "default")
	if err := svc.RemoveSandbox(ctx, "default", "pod-1"); err == nil {
		t.Fatalf("RemoveSandbox() error = nil, want cleanup error")
	}
	rec, err := store.GetSandbox(ctx, "pod-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec.State != state.SandboxUnknown {
		t.Fatalf("sandbox.State = %q, want %q", rec.State, state.SandboxUnknown)
	}
}

func TestStopSandboxDoesNotCreateStateForUnknownRuntime(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("sandbox not found")}
	svc := New(sandboxOps, nil, store, "default")
	if err := svc.StopSandbox(ctx, "default", "missing-pod"); err != nil {
		t.Fatalf("StopSandbox() error = %v", err)
	}

	records, err := store.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("ListSandboxes() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("sandboxes = %#v, want none", records)
	}
}

func TestStopSandboxTerminatesRecordedVMMWhenRuntimeMissing(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	sandboxID := "sandbox-orphan"
	cmd := exec.Command("bash", "-c", "while true; do sleep 1; done", sandboxID)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start test process: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-done
		}
	})

	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: sandboxID,
		Namespace:      "default",
		State:          state.SandboxNotReady,
		VMMPID:         cmd.Process.Pid,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("sandbox not found")}
	svc := New(sandboxOps, nil, store, "default")
	if err := svc.StopSandbox(ctx, "default", "pod-1"); err != nil {
		t.Fatalf("StopSandbox() error = %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("recorded VMM process pid %d still running", cmd.Process.Pid)
	}
}

func TestDeleteSandboxRuntimeStateRemovesLastViewSnapshotRef(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	svc := New(nil, nil, store, "default")
	namespace := "default"
	sandboxID := "sandbox-1"
	parentID := "parent-rootfs"

	if err := store.UpsertSnapshotRuntime(ctx, state.SnapshotRuntimeRecord{
		Namespace: namespace,
		SandboxID: sandboxID,
		State:     state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSnapshotRuntime() error = %v", err)
	}
	if err := store.UpsertViewSnapshot(ctx, state.ViewSnapshotRecord{
		Namespace:        namespace,
		ParentSnapshotID: parentID,
		ViewSnapshotKey:  "view-rootfs",
		RefCount:         1,
		State:            state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertViewSnapshot() error = %v", err)
	}
	if err := store.UpsertViewAlias(ctx, state.ViewAliasRecord{
		Namespace:        namespace,
		AliasKey:         runtimeSnapshot.RootfsViewAliasKey(sandboxID),
		SandboxID:        sandboxID,
		ParentSnapshotID: parentID,
	}); err != nil {
		t.Fatalf("UpsertViewAlias() error = %v", err)
	}

	if err := svc.deleteSandboxRuntimeState(ctx, namespace, sandboxID); err != nil {
		t.Fatalf("deleteSandboxRuntimeState() error = %v", err)
	}

	views, err := store.ListViewSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListViewSnapshots() error = %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("view snapshots = %#v, want none", views)
	}
	aliases, err := store.ListViewAliases(ctx)
	if err != nil {
		t.Fatalf("ListViewAliases() error = %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("view aliases = %#v, want none", aliases)
	}
	runtimes, err := store.ListSnapshotRuntimes(ctx)
	if err != nil {
		t.Fatalf("ListSnapshotRuntimes() error = %v", err)
	}
	if len(runtimes) != 0 {
		t.Fatalf("snapshot runtimes = %#v, want none", runtimes)
	}
}

func TestDeleteSandboxRuntimeStateDecrementsSharedViewSnapshotRef(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	svc := New(nil, nil, store, "default")
	namespace := "default"
	parentID := "parent-rootfs"

	if err := store.UpsertViewSnapshot(ctx, state.ViewSnapshotRecord{
		Namespace:        namespace,
		ParentSnapshotID: parentID,
		ViewSnapshotKey:  "view-rootfs",
		RefCount:         2,
		State:            state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertViewSnapshot() error = %v", err)
	}
	for _, sandboxID := range []string{"sandbox-1", "sandbox-2"} {
		if err := store.UpsertViewAlias(ctx, state.ViewAliasRecord{
			Namespace:        namespace,
			AliasKey:         runtimeSnapshot.RootfsViewAliasKey(sandboxID),
			SandboxID:        sandboxID,
			ParentSnapshotID: parentID,
		}); err != nil {
			t.Fatalf("UpsertViewAlias(%s) error = %v", sandboxID, err)
		}
	}

	if err := svc.deleteSandboxRuntimeState(ctx, namespace, "sandbox-1"); err != nil {
		t.Fatalf("deleteSandboxRuntimeState() error = %v", err)
	}

	views, err := store.ListViewSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListViewSnapshots() error = %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("view snapshots = %#v, want one", views)
	}
	if views[0].ParentSnapshotID != parentID || views[0].RefCount != 1 {
		t.Fatalf("view snapshot = %#v, want parent %q refCount 1", views[0], parentID)
	}
	aliases, err := store.ListViewAliases(ctx)
	if err != nil {
		t.Fatalf("ListViewAliases() error = %v", err)
	}
	if len(aliases) != 1 || aliases[0].AliasKey != runtimeSnapshot.RootfsViewAliasKey("sandbox-2") {
		t.Fatalf("view aliases = %#v, want sandbox-2 alias only", aliases)
	}
}

func TestCreateSandboxAppliesDefaults(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil, "default")
	svc.SetSandboxDefaults(SandboxDefaults{
		ImageName: "registry.example.invalid/conch/sandbox:latest",
		VMMName:   "cloud-hypervisor",
		VCPUNum:   2,
		VCPUMax:   4,
		RamMB:     4096,
	})

	result, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	if sandboxOps.req.ImageName != "registry.example.invalid/conch/sandbox:latest" {
		t.Fatalf("ImageName = %q", sandboxOps.req.ImageName)
	}
	if sandboxOps.req.VmmName != "cloud-hypervisor" {
		t.Fatalf("VmmName = %q", sandboxOps.req.VmmName)
	}
	if sandboxOps.req.VcpuNum != 2 || sandboxOps.req.VcpuMax != 4 || sandboxOps.req.RamMB != 4096 {
		t.Fatalf("resources = vcpu:%d max:%d ram:%d", sandboxOps.req.VcpuNum, sandboxOps.req.VcpuMax, sandboxOps.req.RamMB)
	}
	if sandboxOps.req.AgentToken == "" {
		t.Fatal("AgentToken is empty")
	}
	if result.AgentToken != sandboxOps.req.AgentToken {
		t.Fatalf("result.AgentToken = %q, want generated token", result.AgentToken)
	}
}

func TestCreateSandboxKeepsExplicitOptions(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil, "default")
	svc.SetSandboxDefaults(SandboxDefaults{
		ImageName: "default-image",
		VMMName:   "default-vmm",
		VCPUNum:   2,
		VCPUMax:   2,
		RamMB:     4096,
	})

	_, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
		SandboxID:  "sandbox-1",
		ImageName:  "explicit-image",
		VMMName:    "explicit-vmm",
		VCPUNum:    6,
		VCPUMax:    8,
		RamMB:      8192,
		SnapshotID: "snapshot-id",
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	if sandboxOps.req.ImageName != "explicit-image" || sandboxOps.req.VmmName != "explicit-vmm" {
		t.Fatalf("request = %#v", sandboxOps.req)
	}
	if sandboxOps.req.VcpuNum != 6 || sandboxOps.req.VcpuMax != 8 || sandboxOps.req.RamMB != 8192 {
		t.Fatalf("resources = vcpu:%d max:%d ram:%d", sandboxOps.req.VcpuNum, sandboxOps.req.VcpuMax, sandboxOps.req.RamMB)
	}
}

func TestImageRepoDigests(t *testing.T) {
	tests := []struct {
		name   string
		ref    string
		digest string
		want   []string
	}{
		{
			name:   "tagged image",
			ref:    "registry.example.invalid/conch/demo:latest",
			digest: "sha256:demo",
			want:   []string{"registry.example.invalid/conch/demo@sha256:demo"},
		},
		{
			name:   "repo digest image",
			ref:    "registry.example.invalid/conch/demo@sha256:old",
			digest: "sha256:demo",
			want:   []string{"registry.example.invalid/conch/demo@sha256:demo"},
		},
		{
			name:   "digest only",
			ref:    "sha256:demo",
			digest: "sha256:demo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := imageRepoDigests(tt.ref, tt.digest)
			if len(got) != len(tt.want) {
				t.Fatalf("imageRepoDigests() = %#v, want %#v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("imageRepoDigests()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func newTestStore(t *testing.T) *state.BoltStore {
	t.Helper()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return store
}
