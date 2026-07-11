package cri

import (
	"context"
	"testing"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/runtimeapi"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakeRuntime struct {
	createReq          runtimeapi.SandboxCreateOptions
	createContainerReq runtimeapi.ContainerCreateOptions
	pullImageReq       runtimeapi.PullImageOptions
	listImagesReq      runtimeapi.ListImagesOptions
	removeImageReq     runtimeapi.RemoveImageOptions
	removeImageReqs    []runtimeapi.RemoveImageOptions
	removeImageErrs    map[string]error
	images             []runtimeapi.ImageRecord
	pullResult         runtimeapi.PullImageResult
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

func (f *fakeRuntime) PullImage(_ context.Context, req runtimeapi.PullImageOptions) (runtimeapi.PullImageResult, error) {
	f.pullImageReq = req
	return f.pullResult, nil
}

func (f *fakeRuntime) ListImages(_ context.Context, req runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error) {
	f.listImagesReq = req
	return f.images, nil
}

func (f *fakeRuntime) RemoveImage(_ context.Context, req runtimeapi.RemoveImageOptions) error {
	f.removeImageReq = req
	f.removeImageReqs = append(f.removeImageReqs, req)
	if f.removeImageErrs != nil {
		if err := f.removeImageErrs[req.ImageName]; err != nil {
			return err
		}
	}
	return nil
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
				annotationTemplateID: "tmpl_123",
				annotationVMMName:    "cloud-hypervisor",
				annotationVCPU:       "3",
				annotationVCPUMax:    "5",
				annotationRAMMB:      "2048",
			},
		},
		RuntimeHandler: "conch",
	})
	if err != nil {
		t.Fatalf("RunPodSandbox() error = %v", err)
	}

	if runtime.createReq.TemplateID != "tmpl_123" {
		t.Fatalf("TemplateID = %q", runtime.createReq.TemplateID)
	}
	if runtime.createReq.Namespace != "" {
		t.Fatalf("Namespace = %q, want fixed default runtime namespace", runtime.createReq.Namespace)
	}
	if runtime.createReq.PodNamespace != "team-a" {
		t.Fatalf("PodNamespace = %q, want team-a", runtime.createReq.PodNamespace)
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

	if runtime.createReq.TemplateID != "" || runtime.createReq.VMMName != "" {
		t.Fatalf("request should leave source/vmm defaults to runtime: %#v", runtime.createReq)
	}
	if runtime.createReq.VCPUNum != 0 || runtime.createReq.VCPUMax != 0 || runtime.createReq.RamMB != 0 {
		t.Fatalf("request should leave resource defaults to runtime: %#v", runtime.createReq)
	}
}

func TestSandboxMetadataUsesPodNamespace(t *testing.T) {
	meta := sandboxMetadata(state.SandboxRecord{
		Name:         "pod-a",
		Namespace:    "conch-system",
		PodNamespace: "team-a",
		UID:          "uid-a",
		Attempt:      2,
	})
	if meta.GetNamespace() != "team-a" {
		t.Fatalf("Namespace = %q, want pod namespace team-a", meta.GetNamespace())
	}

	meta = sandboxMetadata(state.SandboxRecord{
		Name:      "pod-a",
		Namespace: "legacy-team-a",
	})
	if meta.GetNamespace() != "legacy-team-a" {
		t.Fatalf("legacy Namespace = %q, want fallback namespace", meta.GetNamespace())
	}
}
