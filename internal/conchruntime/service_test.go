package conchruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

type fakeSandboxOps struct {
	req           sandbox.CreateRequest
	createResult  runtimeapi.SandboxCreateResult
	createErr     error
	createCalls   int
	checkpoint    sandbox.CheckpointResult
	checkpointErr error
}

func (f *fakeSandboxOps) Create(_ context.Context, req sandbox.CreateRequest) (runtimeapi.SandboxCreateResult, error) {
	f.req = req
	f.createCalls++
	result := f.createResult
	if result.SandboxID == "" {
		result.SandboxID = req.SandboxID
	}
	return result, f.createErr
}
func (f *fakeSandboxOps) Delete(context.Context, string) error  { return nil }
func (f *fakeSandboxOps) Suspend(context.Context, string) error { return nil }
func (f *fakeSandboxOps) Resume(context.Context, string) error  { return nil }
func (f *fakeSandboxOps) UpdateNetwork(context.Context, sandbox.NetworkUpdateRequest) error {
	return nil
}
func (f *fakeSandboxOps) Checkpoint(ctx context.Context, _ string, register func(context.Context, sandbox.CheckpointResult) error) (sandbox.CheckpointResult, error) {
	if f.checkpointErr != nil {
		return sandbox.CheckpointResult{}, f.checkpointErr
	}
	return f.checkpoint, register(ctx, f.checkpoint)
}

type fakeTemplateStore struct {
	entries map[string]conchtemplate.Entry
	putHook func(context.Context)
	putErr  error
}

const testTemplateName = "registry.example/conch/test:latest"

func (f *fakeTemplateStore) Put(ctx context.Context, entry conchtemplate.Entry, _ ocispec.Descriptor) (conchtemplate.Entry, error) {
	if f.putHook != nil {
		f.putHook(ctx)
	}
	if f.putErr != nil {
		return conchtemplate.Entry{}, f.putErr
	}
	if f.entries == nil {
		f.entries = make(map[string]conchtemplate.Entry)
	}
	f.entries[entry.Name] = entry
	return entry, nil
}

func (f *fakeTemplateStore) Get(_ context.Context, name string) (conchtemplate.Entry, error) {
	entry, ok := f.entries[name]
	if !ok {
		return conchtemplate.Entry{}, conchtemplate.ErrNotFound.New()
	}
	return entry, nil
}

func (f *fakeTemplateStore) List(context.Context, conchtemplate.Filter) ([]conchtemplate.Entry, error) {
	out := make([]conchtemplate.Entry, 0, len(f.entries))
	for _, entry := range f.entries {
		out = append(out, entry)
	}
	return out, nil
}

func (f *fakeTemplateStore) Delete(_ context.Context, name string) error {
	delete(f.entries, name)
	return nil
}

func setFakeTemplate(svc *Service, name, templateID string, mode conchtemplate.BootMode) {
	svc.Templates = &fakeTemplateStore{entries: map[string]conchtemplate.Entry{
		name: {
			Name:            name,
			Origin:          conchtemplate.OriginImage,
			BootMode:        mode,
			BootIndexDigest: templateID,
		},
	}}
}

func TestTemplateRecordUsesBootIndexDigestsAsTemplateIDs(t *testing.T) {
	id := digest.FromString("template").String()
	parentID := digest.FromString("parent-template").String()

	record := publicTemplateRecord(conchtemplate.Entry{
		Name:                  "registry.example/conch/template:latest",
		Origin:                conchtemplate.OriginCheckpoint,
		BootMode:              conchtemplate.BootModeResume,
		BootIndexDigest:       id,
		ParentBootIndexDigest: parentID,
	})

	if record.TemplateID != id {
		t.Fatalf("TemplateID = %q, want Boot Index digest %q", record.TemplateID, id)
	}
	if record.Name != "registry.example/conch/template:latest" {
		t.Fatalf("Name = %q", record.Name)
	}
	if record.ParentTemplateID != parentID {
		t.Fatalf("ParentTemplateID = %q, want parent Boot Index digest %q", record.ParentTemplateID, parentID)
	}
}

func TestCreateSandboxAppliesConfiguredBackend(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil)
	defaultDigest := digest.FromString("default-template").String()
	svc.SetSandboxDefaults(SandboxDefaults{
		TemplateName: testTemplateName,
		VMMName:      "cloud-hypervisor",
		VCPUNum:      2,
		VCPUMax:      4,
		RamMB:        4096,
	})
	setFakeTemplate(svc, testTemplateName, defaultDigest, conchtemplate.BootModeCold)

	result, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
		SandboxID: "sandbox-1",
		Env:       map[string]string{"SOME_RANDOM_KEY": "key123"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	if sandboxOps.req.TemplateID != defaultDigest {
		t.Fatalf("TemplateID = %q", sandboxOps.req.TemplateID)
	}
	if sandboxOps.req.VMMName != "cloud-hypervisor" {
		t.Fatalf("VmmName = %q", sandboxOps.req.VMMName)
	}
	if sandboxOps.req.VCPUNum != 2 || sandboxOps.req.VCPUMax != 4 || sandboxOps.req.RAMMB != 4096 {
		t.Fatalf("resources = vcpu:%d max:%d ram:%d", sandboxOps.req.VCPUNum, sandboxOps.req.VCPUMax, sandboxOps.req.RAMMB)
	}
	if got := sandboxOps.req.Env["SOME_RANDOM_KEY"]; got != "key123" {
		t.Fatalf("Env[SOME_RANDOM_KEY] = %q, want key123", got)
	}
	if result.AgentToken != sandboxOps.req.AgentToken {
		t.Fatalf("result.AgentToken = %q, want generated token", result.AgentToken)
	}
	if result.SandboxID != "sandbox-1" || sandboxOps.req.SandboxID != "sandbox-1" {
		t.Fatalf("sandbox identity = result:%q request:%q", result.SandboxID, sandboxOps.req.SandboxID)
	}
}

func TestCreateSandboxRejectsMissingTemplate(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil)
	_, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{TemplateName: " \n ", VCPUNum: 2, VCPUMax: 2, RamMB: 1024})
	if !errors.Is(err, sandbox.ErrInvalidArgument) {
		t.Fatalf("CreateSandbox() error = %v, want sandbox.ErrInvalidArgument", err)
	}
}

func TestCreateSandboxRejectsUnknownTemplateName(t *testing.T) {
	operations := &fakeSandboxOps{}
	service := New(operations, nil)
	service.Templates = &fakeTemplateStore{entries: map[string]conchtemplate.Entry{}}

	_, err := service.CreateSandbox(context.Background(), SandboxCreateOptions{
		TemplateName: "registry.example/conch/missing:latest",
	})
	if !errors.Is(err, conchtemplate.ErrNotFound) {
		t.Fatalf("CreateSandbox() error = %v, want template.ErrNotFound", err)
	}
	if operations.createCalls != 0 {
		t.Fatalf("runtime Create() calls = %d, want 0", operations.createCalls)
	}
}

func TestCreateSandboxRejectsTemplateNameAndIDTogether(t *testing.T) {
	operations := &fakeSandboxOps{}
	service := New(operations, nil)

	_, err := service.CreateSandbox(context.Background(), SandboxCreateOptions{
		TemplateName: "registry.example/conch/template:latest",
		TemplateID:   digest.FromString("template").String(),
	})
	if !errors.Is(err, sandbox.ErrInvalidArgument) || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	if operations.createCalls != 0 {
		t.Fatalf("runtime Create() calls = %d, want 0", operations.createCalls)
	}
}

func TestCreateSandboxRejectsInvalidTemplateID(t *testing.T) {
	operations := &fakeSandboxOps{}
	service := New(operations, nil)

	_, err := service.CreateSandbox(context.Background(), SandboxCreateOptions{TemplateID: "not-a-digest"})
	if !errors.Is(err, sandbox.ErrInvalidArgument) {
		t.Fatalf("CreateSandbox() error = %v, want ErrInvalidArgument", err)
	}
	if operations.createCalls != 0 {
		t.Fatalf("runtime Create() calls = %d, want 0", operations.createCalls)
	}
}

func TestCreateSandboxKeepsExplicitOptions(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil)
	defaultDigest := digest.FromString("default-template").String()
	explicitDigest := digest.FromString("resume-template").String()
	const explicitName = "registry.example/conch/resume:latest"
	svc.SetSandboxDefaults(SandboxDefaults{
		TemplateID: defaultDigest,
		VMMName:    "default-vmm",
		VCPUNum:    2,
		VCPUMax:    2,
		RamMB:      4096,
	})
	svc.Templates = &fakeTemplateStore{entries: map[string]conchtemplate.Entry{
		explicitName: {Name: explicitName, Origin: conchtemplate.OriginCheckpoint, BootMode: conchtemplate.BootModeResume, BootIndexDigest: explicitDigest},
	}}

	_, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
		SandboxID:    "sandbox-1",
		TemplateName: explicitName,
		VMMName:      "explicit-vmm",
		VCPUNum:      6,
		VCPUMax:      8,
		RamMB:        8192,
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	if sandboxOps.req.TemplateID != explicitDigest || sandboxOps.req.VMMName != "explicit-vmm" {
		t.Fatalf("request = %#v", sandboxOps.req)
	}
	if sandboxOps.req.VCPUNum != 6 || sandboxOps.req.VCPUMax != 8 || sandboxOps.req.RAMMB != 8192 {
		t.Fatalf("resources = vcpu:%d max:%d ram:%d", sandboxOps.req.VCPUNum, sandboxOps.req.VCPUMax, sandboxOps.req.RAMMB)
	}
}

func TestCreateTemplateRequiresContainerdClient(t *testing.T) {
	svc := New(nil, nil)

	if _, err := svc.CreateTemplate(context.Background(), TemplateCreateOptions{}); err == nil || err.Error() != "containerd client is required" {
		t.Fatalf("CreateTemplate() error = %v, want containerd client is required", err)
	}
}

func TestCreateTemplateRejectsTemplateSource(t *testing.T) {
	ctx := context.Background()
	host := newRuntimeImageHost(t)
	bootIndexDigest := buildColdBootIndex(t, host, "canonical-rootfs-source")
	const sourceName = "registry.example/conch/template-source:latest"
	seedTemplate(t, ctx, host, sourceName, bootIndexDigest, conchtemplate.BootModeCold)

	svc := New(nil, host.Client())
	svc.Templates = host.TemplateStore()
	_, err := svc.CreateTemplate(ctx, TemplateCreateOptions{
		Name:       "registry.example/conch/template-output:latest",
		Source:     conchimage.TemplateRecordName(sourceName),
		KernelPath: "unused-kernel",
		InitrdPath: "unused-initrd",
	})
	if !errors.Is(err, conchtemplate.ErrInvalidArgument) {
		t.Fatalf("CreateTemplate() error = %v, want ErrInvalidArgument", err)
	}

	record, err := host.Client().ImageService().Get(containerdclient.NewNamespaceContext(ctx), conchimage.TemplateRecordName(sourceName))
	if err != nil {
		t.Fatalf("get named Template image: %v", err)
	}
	if got := record.Labels[conchimage.ImageKindLabel]; got != conchimage.ImageKindBootIndexCold {
		t.Fatalf("Template image kind = %q, want %q", got, conchimage.ImageKindBootIndexCold)
	}
	if _, err := svc.Templates.Get(ctx, sourceName); err != nil {
		t.Fatalf("Get() original Template after rejected create: %v", err)
	}
}

func TestUnpackTemplateResolvesBootIndexByName(t *testing.T) {
	ctx := context.Background()
	host := newRuntimeImageHost(t)
	bootIndexDigest := buildColdBootIndex(t, host, "explicit-unpack")
	svc := New(nil, host.Client())
	svc.Templates = host.TemplateStore()

	const templateName = "registry.example/conch/unpack:latest"
	if _, err := svc.Templates.Put(ctx, conchtemplate.Entry{
		Name:            templateName,
		Origin:          conchtemplate.OriginImage,
		BootMode:        conchtemplate.BootModeCold,
		BootIndexDigest: bootIndexDigest,
		SourceRef:       "not-the-boot-index:latest",
	}, bootIndexTarget(t, host, bootIndexDigest)); err != nil {
		t.Fatalf("create template: %v", err)
	}

	if err := svc.UnpackTemplate(ctx, TemplateUnpackOptions{Name: templateName}); err != nil {
		t.Fatalf("UnpackTemplate() error = %v", err)
	}
}

func newRuntimeImageHost(t *testing.T) *containerdhost.Host {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required")
	}
	host, err := containerdhost.Start(context.Background(), containerdhost.Config{
		RootDir:  t.TempDir(),
		StateDir: t.TempDir(),
		Snapshot: containerdhost.SnapshotConfig{
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		t.Skipf("embedded containerd host unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Errorf("close containerd host: %v", err)
		}
	})
	return host
}

func buildColdBootIndex(t *testing.T, host *containerdhost.Host, name string) string {
	t.Helper()
	ctx := containerdclient.NewNamespaceContext(context.Background())
	leaseCtx, done, err := host.Client().WithLease(ctx)
	if err != nil {
		t.Fatalf("create source boot index lease: %v", err)
	}
	t.Cleanup(func() { done(leaseCtx) })
	ctx = leaseCtx
	store := host.Client().ContentStore()
	rootfsDir := filepath.Join(t.TempDir(), "rootfs")
	sandboxDir := filepath.Join(t.TempDir(), "sandbox")
	for _, dir := range []string{rootfsDir, sandboxDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rootfsDesc, err := conchimage.BuildNativeComponentInContent(
		ctx, store, []string{rootfsDir}, conchimage.KindRootfs,
	)
	if err != nil {
		t.Fatalf("build rootfs component: %v", err)
	}
	sandboxDesc, err := conchimage.BuildNativeComponentInContent(
		ctx, store, []string{sandboxDir}, conchimage.KindSandbox,
	)
	if err != nil {
		t.Fatalf("build sandbox component: %v", err)
	}
	indexDesc, err := conchimage.BuildBootIndexInContent(ctx, store, conchimage.BootIndexContentOptions{
		RootfsDescriptor:  rootfsDesc,
		SandboxDescriptor: sandboxDesc,
	})
	if err != nil {
		t.Fatalf("build cold boot index: %v", err)
	}
	return indexDesc.Digest.String()
}

func seedTemplate(
	t *testing.T,
	ctx context.Context,
	host *containerdhost.Host,
	name string,
	bootIndexDigest string,
	bootMode conchtemplate.BootMode,
) {
	t.Helper()
	if _, err := host.TemplateStore().Put(ctx, conchtemplate.Entry{
		Name:            name,
		Origin:          conchtemplate.OriginImage,
		BootMode:        bootMode,
		BootIndexDigest: bootIndexDigest,
	}, bootIndexTarget(t, host, bootIndexDigest)); err != nil {
		t.Fatalf("PutTemplate(%s, %s) error = %v", name, bootIndexDigest, err)
	}
}

func bootIndexTarget(t *testing.T, host *containerdhost.Host, bootIndexDigest string) ocispec.Descriptor {
	t.Helper()
	info, err := host.Client().ContentStore().Info(
		containerdclient.NewNamespaceContext(context.Background()), digest.Digest(bootIndexDigest),
	)
	if err != nil {
		t.Fatalf("resolve Boot Index %s: %v", bootIndexDigest, err)
	}
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.Digest(bootIndexDigest),
		Size:      info.Size,
	}
}

func TestCreateResolvesTemplateWithoutContentAccess(t *testing.T) {
	for _, byName := range []bool{true, false} {
		t.Run(map[bool]string{true: "name", false: "digest"}[byName], func(t *testing.T) {
			want := digest.FromString("template").String()
			result := runtimeapi.SandboxCreateResult{SandboxID: "created", TemplateID: want, AgentToken: "runtime-token"}
			ops := &fakeSandboxOps{createResult: result}
			svc := New(ops, nil)
			setFakeTemplate(svc, testTemplateName, want, conchtemplate.BootModeCold)
			opts := SandboxCreateOptions{TemplateID: want}
			if byName {
				opts.TemplateID = ""
				opts.TemplateName = testTemplateName
			}
			got, err := svc.CreateSandbox(context.Background(), opts)
			if err != nil || got != result || ops.req.TemplateID != want || ops.createCalls != 1 {
				t.Fatalf("Create=%#v, %v, req=%#v", got, err, ops.req)
			}
		})
	}
}

func TestCheckpointRegistersPublishedArtifact(t *testing.T) {
	target := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromString("checkpoint")}
	result := sandbox.CheckpointResult{BootIndexDigest: target.Digest.String(), ParentBootIndexDigest: digest.FromString("parent").String(), Target: target}
	ops := &fakeSandboxOps{checkpoint: result}
	svc := New(ops, nil)
	templates := &fakeTemplateStore{}
	svc.Templates = templates
	got, err := svc.CheckpointSandbox(context.Background(), SandboxCheckpointOptions{SandboxID: "sandbox-a", TemplateName: "snapshot:latest"})
	if err != nil || got.TemplateID != result.BootIndexDigest {
		t.Fatalf("Checkpoint=%#v, %v", got, err)
	}
	entry := templates.entries["snapshot:latest"]
	if entry.ParentBootIndexDigest != result.ParentBootIndexDigest || entry.SourceSandboxID != "sandbox-a" || entry.BootMode != conchtemplate.BootModeResume {
		t.Fatalf("registered=%#v", entry)
	}
}
