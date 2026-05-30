package cri

import (
	"context"
	"testing"

	"github.com/openeuler/Conch/internal/runtimeapi"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakeRuntime struct {
	createReq          runtimeapi.SandboxCreateOptions
	createContainerReq runtimeapi.ContainerCreateOptions
}

func (f *fakeRuntime) CreateSandbox(_ context.Context, req runtimeapi.SandboxCreateOptions) (runtimeapi.SandboxCreateResult, error) {
	f.createReq = req
	return runtimeapi.SandboxCreateResult{PodSandboxID: req.PodSandboxID, SandboxID: req.SandboxID, Namespace: req.Namespace}, nil
}

func (f *fakeRuntime) StopSandbox(context.Context, string, string) error {
	return nil
}

func (f *fakeRuntime) RemoveSandbox(context.Context, string, string) error {
	return nil
}

func (f *fakeRuntime) CreateContainer(_ context.Context, req runtimeapi.ContainerCreateOptions) (runtimeapi.ContainerCreateResult, error) {
	f.createContainerReq = req
	return runtimeapi.ContainerCreateResult{ContainerID: "container-1"}, nil
}

func (f *fakeRuntime) SetContainerState(context.Context, string, string) error {
	return nil
}

func (f *fakeRuntime) PullImage(context.Context, runtimeapi.PullImageOptions) (runtimeapi.PullImageResult, error) {
	return runtimeapi.PullImageResult{}, nil
}

func TestRunPodSandboxPassesConchAnnotations(t *testing.T) {
	runtime := &fakeRuntime{}
	svc := &service{runtime: runtime}

	_, err := svc.RunPodSandbox(context.Background(), &runtimev1.RunPodSandboxRequest{
		Config: &runtimev1.PodSandboxConfig{
			Metadata: &runtimev1.PodSandboxMetadata{
				Name:      "pod-a",
				Namespace: "team-a",
				Uid:       "uid-a",
				Attempt:   2,
			},
			Annotations: map[string]string{
				annotationSandboxImage: "registry.example.invalid/conch/sandbox:latest",
				annotationUseSnapshot:  "true",
				annotationVMMName:      "cloud-hypervisor",
				annotationVCPU:         "3",
				annotationVCPUMax:      "5",
				annotationRAMMB:        "2048",
			},
		},
		RuntimeHandler: "conch",
	})
	if err != nil {
		t.Fatalf("RunPodSandbox() error = %v", err)
	}

	if runtime.createReq.ImageName != "registry.example.invalid/conch/sandbox:latest" {
		t.Fatalf("ImageName = %q", runtime.createReq.ImageName)
	}
	if !runtime.createReq.UseSnapshot {
		t.Fatalf("UseSnapshot = false, want true")
	}
	if runtime.createReq.VMMName != "cloud-hypervisor" {
		t.Fatalf("VMMName = %q", runtime.createReq.VMMName)
	}
	if runtime.createReq.VCPUNum != 3 || runtime.createReq.VCPUMax != 5 || runtime.createReq.RamMB != 2048 {
		t.Fatalf("resources = vcpu:%d max:%d ram:%d", runtime.createReq.VCPUNum, runtime.createReq.VCPUMax, runtime.createReq.RamMB)
	}
}

func TestRunPodSandboxLeavesDefaultsToRuntime(t *testing.T) {
	runtime := &fakeRuntime{}
	svc := &service{runtime: runtime}

	_, err := svc.RunPodSandbox(context.Background(), &runtimev1.RunPodSandboxRequest{
		Config: &runtimev1.PodSandboxConfig{
			Metadata: &runtimev1.PodSandboxMetadata{Name: "pod-a", Namespace: "team-a"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox() error = %v", err)
	}

	if runtime.createReq.ImageName != "" || runtime.createReq.VMMName != "" {
		t.Fatalf("request should leave image/vmm defaults to runtime: %#v", runtime.createReq)
	}
	if runtime.createReq.VCPUNum != 0 || runtime.createReq.VCPUMax != 0 || runtime.createReq.RamMB != 0 {
		t.Fatalf("request should leave resource defaults to runtime: %#v", runtime.createReq)
	}
}
