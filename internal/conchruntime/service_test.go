package conchruntime

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/sandbox"
)

type fakeSandboxOps struct {
	req           sandbox.SandboxCreateRequest
	checkpointReq sandbox.SandboxCheckpointRequest
	createResult  sandbox.SandboxCreateResult
	deleteErr     error
}

func (f *fakeSandboxOps) Create(req sandbox.SandboxCreateRequest) (sandbox.SandboxCreateResult, error) {
	f.req = req
	result := f.createResult
	if result.Namespace == "" {
		result.Namespace = req.Namespace
	}
	if result.SandboxID == "" {
		result.SandboxID = req.SandboxId
	}
	if result.IP == "" {
		result.IP = "192.0.2.10"
	}
	if result.AgentToken == "" {
		result.AgentToken = req.AgentToken
	}
	return result, nil
}

func (f *fakeSandboxOps) Delete(sandbox.SandboxDeleteRequest) error {
	return f.deleteErr
}

func (f *fakeSandboxOps) Suspend(sandbox.SandboxLifecycleRequest) error {
	return nil
}

func (f *fakeSandboxOps) Resume(sandbox.SandboxLifecycleRequest) error {
	return nil
}

func (f *fakeSandboxOps) Checkpoint(req sandbox.SandboxCheckpointRequest) (sandbox.SandboxCheckpointResult, error) {
	f.checkpointReq = req
	return sandbox.SandboxCheckpointResult{RootfsKey: "rootfs", MemKey: "mem", VMKey: "vm"}, nil
}

func TestCheckpointSandboxPassesTemplateIDToSandbox(t *testing.T) {
	ctx := context.Background()
	sandboxOps := &fakeSandboxOps{}
	store := newTestStore(t)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "sandbox-a",
		ConchSandboxID: "sandbox-a",
		Namespace:      "team-a",
		ParentVMID:     "vm-parent",
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	svc := New(sandboxOps, nil, store, "default")

	result, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{
		Namespace:    "team-a",
		PodSandboxID: "sandbox-a",
	})
	if err != nil {
		t.Fatalf("CheckpointSandbox() error = %v", err)
	}
	if result.TemplateID == "" {
		t.Fatal("CheckpointSandbox() returned an empty template id")
	}
	if sandboxOps.checkpointReq.TemplateID != result.TemplateID {
		t.Fatalf("checkpoint template id = %q, want %q", sandboxOps.checkpointReq.TemplateID, result.TemplateID)
	}
	if sandboxOps.checkpointReq.ParentVMID != "vm-parent" {
		t.Fatalf("checkpoint parent VM id = %q, want vm-parent", sandboxOps.checkpointReq.ParentVMID)
	}
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

func TestCreateSandboxStoresRuntimeFieldsOnSandboxRecord(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	sandboxOps := &fakeSandboxOps{
		createResult: sandbox.SandboxCreateResult{
			RootfsKey:      "sandbox-1",
			MemKey:         "sandbox-1-mem",
			ParentRootfsID: "tmpl-rootfs",
			ParentMemID:    "ckpt-mem",
			ParentVMID:     "tmpl-vm",
			RootfsMount:    "/run/conch/rootfs",
			MemMount:       "/run/conch/mem",
			VMMount:        "/run/conch/vm",
			RootDir:        "conch/snapshot",
			MemSize:        512,
		},
	}
	svc := New(sandboxOps, nil, store, "default")

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		PodSandboxID: "pod-1",
		SandboxID:    "sandbox-1",
		TemplateID:   "tmpl-1",
	}); err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	rec, err := store.GetSandbox(ctx, "pod-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec.RootfsKey != "sandbox-1" || rec.MemKey != "sandbox-1-mem" {
		t.Fatalf("runtime keys = (%q, %q), want sandbox keys", rec.RootfsKey, rec.MemKey)
	}
	if rec.ParentRootfsID != "tmpl-rootfs" || rec.ParentMemID != "ckpt-mem" || rec.ParentVMID != "tmpl-vm" {
		t.Fatalf("parent refs = (%q, %q, %q)", rec.ParentRootfsID, rec.ParentMemID, rec.ParentVMID)
	}
	if rec.RootfsMount != "/run/conch/rootfs" || rec.MemMount != "/run/conch/mem" || rec.VMMount != "/run/conch/vm" {
		t.Fatalf("mounts = (%q, %q, %q)", rec.RootfsMount, rec.MemMount, rec.VMMount)
	}
	if rec.SnapshotRootDir != "conch/snapshot" {
		t.Fatalf("SnapshotRootDir = %q", rec.SnapshotRootDir)
	}
}

func TestCreateSandboxAppliesDefaults(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil, "default")
	svc.SetSandboxDefaults(SandboxDefaults{
		TemplateID: "tmpl_default",
		VMMName:    "cloud-hypervisor",
		VCPUNum:    2,
		VCPUMax:    4,
		RamMB:      4096,
	})

	result, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	if sandboxOps.req.TemplateID != "tmpl_default" {
		t.Fatalf("TemplateID = %q", sandboxOps.req.TemplateID)
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
		TemplateID: "tmpl_default",
		VMMName:    "default-vmm",
		VCPUNum:    2,
		VCPUMax:    2,
		RamMB:      4096,
	})

	_, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
		SandboxID:  "sandbox-1",
		TemplateID: "tmpl_resume_explicit",
		VMMName:    "explicit-vmm",
		VCPUNum:    6,
		VCPUMax:    8,
		RamMB:      8192,
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	if sandboxOps.req.TemplateID != "tmpl_resume_explicit" || sandboxOps.req.VmmName != "explicit-vmm" {
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
