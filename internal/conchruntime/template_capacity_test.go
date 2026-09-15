package conchruntime

import (
	"context"
	"errors"
	"testing"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

func TestResumeTemplateCapturedResourcesOverrideRequest(t *testing.T) {
	ctx := containerdclient.NewNamespaceContext(context.Background())
	host := newRuntimeImageHost(t)
	cold := buildColdBootIndex(t, host, "capacity-cold")
	published, err := conchimage.PublishCheckpointBootIndex(ctx, host.Client(), conchimage.PublishCheckpointBootIndexOptions{
		SourceBootIndexDigest: cold, MemRoot: t.TempDir(), VMMName: "cloud-hypervisor", MemorySizeMB: 512, CPUCount: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	const name = "capacity-resume"
	seedTemplate(t, ctx, host, name, published.BootIndexDigest, conchtemplate.BootModeResume)
	ops := &fakeSandboxOps{}
	svc := New(ops, host.Client())
	svc.Templates = host.TemplateStore()
	svc.SetSandboxDefaults(SandboxDefaults{VMMName: "cloud-hypervisor", VCPUNum: 2, VCPUMax: 2, RamMB: 128})

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{TemplateName: name}); err != nil {
		t.Fatal(err)
	}
	if ops.req.VCPUNum != 8 || ops.req.VCPUMax < 8 || ops.req.RAMMB != 512 {
		t.Fatalf("runtime allocation=%+v", ops.req)
	}
}

func TestResumeTemplateWithoutCapturedCPURejectedForE2B(t *testing.T) {
	// Resume indexes published before CPU capture carry no cpu-count
	// annotation. E2B requests reject them up front instead of restoring
	// with an unverified CPU count; plain requests still boot them using
	// the caller's resource values.
	ctx := containerdclient.NewNamespaceContext(context.Background())
	host := newRuntimeImageHost(t)
	cold := buildColdBootIndex(t, host, "legacy-cold")
	source, err := conchimage.InspectBootIndex(ctx, host.Client(), cold)
	if err != nil {
		t.Fatal(err)
	}
	memDesc, err := conchimage.BuildNativeComponentInContent(ctx, host.Client().ContentStore(), []string{t.TempDir()}, conchimage.KindMemSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	legacyDesc, err := conchimage.BuildBootIndexInContent(ctx, host.Client().ContentStore(), conchimage.BootIndexContentOptions{
		RootfsDescriptor:  source.RootfsDescriptor,
		MemDescriptor:     memDesc,
		SandboxDescriptor: source.SandboxDescriptor,
		VMMName:           "cloud-hypervisor",
		MemorySizeMB:      512,
	})
	if err != nil {
		t.Fatal(err)
	}
	const name = "legacy-resume"
	seedTemplate(t, ctx, host, name, legacyDesc.Digest.String(), conchtemplate.BootModeResume)
	ops := &fakeSandboxOps{}
	svc := New(ops, host.Client())
	svc.Templates = host.TemplateStore()
	svc.SetSandboxDefaults(SandboxDefaults{VMMName: "cloud-hypervisor", VCPUNum: 2, VCPUMax: 2, RamMB: 512})

	_, err = svc.CreateSandbox(ctx, SandboxCreateOptions{TemplateName: name, E2B: true})
	if !errors.Is(err, sandbox.ErrFailedPrecondition) || ops.createCalls != 0 {
		t.Fatalf("err=%v createCalls=%d, want ErrFailedPrecondition with no runtime create", err, ops.createCalls)
	}
	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{TemplateName: name}); err != nil {
		t.Fatalf("non-E2B create with legacy resume template: %v", err)
	}
}
