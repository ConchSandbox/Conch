package conchruntime

import (
	"context"
	"testing"

	"github.com/openeuler/Conch/internal/sandbox"
)

type fakeSandboxOps struct {
	req sandbox.SandboxCreateRequest
}

func (f *fakeSandboxOps) Create(req sandbox.SandboxCreateRequest) (string, error) {
	f.req = req
	return "192.0.2.10", nil
}

func (f *fakeSandboxOps) Delete(sandbox.SandboxDeleteRequest) error {
	return nil
}

func (f *fakeSandboxOps) Pause(sandbox.SandboxPauseRequest) (string, error) {
	return "", nil
}

func TestCreateSandboxAppliesDefaults(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, "default")
	svc.SetSandboxDefaults(SandboxDefaults{
		ImageName: "registry.example.invalid/conch/sandbox:latest",
		VMMName:   "cloud-hypervisor",
		VCPUNum:   2,
		VCPUMax:   4,
		RamMB:     4096,
	})

	if _, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{SandboxID: "sandbox-1"}); err != nil {
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
}

func TestCreateSandboxKeepsExplicitOptions(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, "default")
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
