package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/vmm"
)

// retireFixture seeds a READY E2B sandbox whose checkpoint head is a
// resume-capable boot index, so retireMemRecord sees a real mem component.
func retireFixture(t *testing.T) (*Manager, *memorySandboxStore, *recordingImageService, ocispec.Descriptor, Record) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required for checkpoint content")
	}
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	imageSvc, ok := m.client.ImageService().(*recordingImageService)
	if !ok {
		t.Fatalf("test client image service = %T, want *recordingImageService", m.client.ImageService())
	}
	var components []ocispec.Descriptor
	for _, kind := range []string{conchimage.KindRootfs, conchimage.KindMemSnapshot, conchimage.KindSandbox} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "payload"), []byte(kind), 0600); err != nil {
			t.Fatal(err)
		}
		d, err := conchimage.BuildNativeComponentInContent(context.Background(), m.client.ContentStore(), []string{dir}, kind)
		if err != nil {
			t.Fatal(err)
		}
		components = append(components, d)
	}
	target, err := conchimage.BuildBootIndexInContent(context.Background(), m.client.ContentStore(), conchimage.BootIndexContentOptions{
		RootfsDescriptor: components[0], MemDescriptor: components[1], SandboxDescriptor: components[2],
		VMMName: "cloud-hypervisor", MemorySizeMB: 512, CPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := testCreateRequest()
	rec, err := store.Create(context.Background(), Record{
		ID: req.SandboxID, State: StateReady, E2B: true,
		SourceTemplateID: req.TemplateID, CheckpointHeadTemplateID: target.Digest.String(),
		VCPUNum: req.VCPUNum, RamMB: req.RAMMB, StartedAt: 111, ExpiresAt: 999, IP: "10.0.0.2", VMMPID: 4321,
		HeadStartedAt: 55, HeadCreatedAt: 66, TimeoutAction: runtimeapi.TimeoutActionPause,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mem.img"), []byte("memory"), 0600); err != nil {
		t.Fatal(err)
	}
	m.checkpointCapture = &recordingCheckpointCapture{result: CapturedBootComponents{MemRootPath: dir, VMMName: "cloud-hypervisor", MemorySizeMB: 512}}
	m.Routes, m.Capacity, m.Envd = newFakeRouteTable(), &fakeCapacityLedger{}, &fakeEnvdBootstrapper{version: "1.2.3"}
	// Mirror the create-time side effects the seeded record stands for.
	if err := m.Capacity.(*fakeCapacityLedger).Reserve(req.SandboxID, req.VCPUNum, req.RAMMB); err != nil {
		t.Fatal(err)
	}
	m.Routes.(*fakeRouteTable).Insert(req.SandboxID)
	m.sandboxes.Store(req.SandboxID, &sandboxEntry{state: StateReady, sbx: &Sandbox{process: &vmm.Process{}}})
	return m, store, imageSvc, components[1], rec
}

// reReadyForResume mirrors the record and runtime effects of a connect so the
// fixture sandbox can pause again.
func reReadyForResume(t *testing.T, m *Manager, store *memorySandboxStore, sandboxID string) {
	t.Helper()
	ctx := context.Background()
	rec, err := store.Get(ctx, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	rec.State = StateReady
	rec.IP, rec.VMMPID = "10.0.0.2", 0
	if _, err := store.Update(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := m.Capacity.(*fakeCapacityLedger).Reserve(sandboxID, rec.VCPUNum, rec.RamMB); err != nil {
		t.Fatal(err)
	}
	m.Routes.(*fakeRouteTable).Insert(sandboxID)
	m.sandboxes.Store(sandboxID, &sandboxEntry{state: StateReady, sbx: &Sandbox{process: &vmm.Process{}}})
}

func TestPauseRetiresParentMemComponentRecord(t *testing.T) {
	m, _, imageSvc, memDesc, original := retireFixture(t)
	ctx := context.Background()

	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	want := conchimage.ComponentImageName(conchimage.KindMemSnapshot, memDesc.Digest)
	if got := imageSvc.deletedNames(); len(got) != 1 || got[0] != want {
		t.Fatalf("deleted image records = %v, want [%s]", got, want)
	}
	rec, err := m.store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.CheckpointHeadTemplateID == original.CheckpointHeadTemplateID {
		t.Fatalf("checkpoint head did not move: %#v", rec)
	}
}

// freshMemCapture produces a fresh MemRoot per capture; Pause deletes the
// captured directory after publishing.
type freshMemCapture struct {
	t *testing.T
}

func (f *freshMemCapture) Capture(_ context.Context, _ RuntimeCaptureRequest) (CapturedBootComponents, error) {
	dir := f.t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mem.img"), []byte("memory"), 0600); err != nil {
		return CapturedBootComponents{}, err
	}
	return CapturedBootComponents{MemRootPath: dir, VMMName: "cloud-hypervisor", MemorySizeMB: 512}, nil
}

func TestSecondPauseRetiresSupersededMemComponentRecord(t *testing.T) {
	m, store, imageSvc, memDesc, _ := retireFixture(t)
	m.checkpointCapture = &freshMemCapture{t: t}
	ctx := context.Background()

	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatalf("first Pause() error = %v", err)
	}
	firstRec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	firstHead, err := conchimage.InspectBootIndex(ctx, m.client, firstRec.CheckpointHeadTemplateID)
	if err != nil {
		t.Fatal(err)
	}
	if firstHead.MemDescriptor.Digest == memDesc.Digest {
		t.Fatal("published checkpoint reuses the parent mem component")
	}

	reReadyForResume(t, m, store, "sandbox-a")
	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatalf("second Pause() error = %v", err)
	}
	firstGen := conchimage.ComponentImageName(conchimage.KindMemSnapshot, memDesc.Digest)
	secondGen := conchimage.ComponentImageName(conchimage.KindMemSnapshot, firstHead.MemDescriptor.Digest)
	got := imageSvc.deletedNames()
	if len(got) != 2 || got[0] != firstGen || got[1] != secondGen {
		t.Fatalf("deleted image records = %v, want [%s %s]", got, firstGen, secondGen)
	}
	secondRec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	current, err := conchimage.InspectBootIndex(ctx, m.client, secondRec.CheckpointHeadTemplateID)
	if err != nil {
		t.Fatal(err)
	}
	currentName := conchimage.ComponentImageName(conchimage.KindMemSnapshot, current.MemDescriptor.Digest)
	for _, name := range got {
		if name == currentName {
			t.Fatalf("retire deleted the current head's mem component record %s", name)
		}
	}
}

func TestDeleteRetiresMemComponentRecord(t *testing.T) {
	m, store, imageSvc, memDesc, _ := retireFixture(t)
	ctx := context.Background()

	if err := m.Delete(ctx, "sandbox-a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	want := conchimage.ComponentImageName(conchimage.KindMemSnapshot, memDesc.Digest)
	if got := imageSvc.deletedNames(); len(got) != 1 || got[0] != want {
		t.Fatalf("deleted image records = %v, want [%s]", got, want)
	}
	if _, err := store.Get(ctx, "sandbox-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record remains: %v", err)
	}
}

func TestDeleteSkipsRetireForSourceTemplateHead(t *testing.T) {
	m, store, imageSvc, _, _ := retireFixture(t)
	ctx := context.Background()
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	rec.CheckpointHeadTemplateID = rec.SourceTemplateID
	if _, err := store.Update(ctx, rec); err != nil {
		t.Fatal(err)
	}

	if err := m.Delete(ctx, "sandbox-a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got := imageSvc.deletedNames(); len(got) != 0 {
		t.Fatalf("source-template head retired image records = %v, want none", got)
	}
}

func TestDeleteToleratesMissingMemComponentRecord(t *testing.T) {
	m, _, imageSvc, _, _ := retireFixture(t)
	imageSvc.deleteErr = errdefs.ErrNotFound

	if err := m.Delete(context.Background(), "sandbox-a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestCheckpointRetiresParentMemComponentRecord(t *testing.T) {
	m, store, imageSvc, memDesc, original := retireFixture(t)
	ctx := context.Background()

	registered := false
	if _, err := m.Checkpoint(ctx, "sandbox-a", func(context.Context, CheckpointResult) error {
		registered = true
		return nil
	}); err != nil {
		t.Fatalf("Checkpoint() error = %v", err)
	}
	if !registered {
		t.Fatal("register was not called")
	}
	want := conchimage.ComponentImageName(conchimage.KindMemSnapshot, memDesc.Digest)
	if got := imageSvc.deletedNames(); len(got) != 1 || got[0] != want {
		t.Fatalf("deleted image records = %v, want [%s]", got, want)
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.CheckpointHeadTemplateID == original.CheckpointHeadTemplateID {
		t.Fatalf("checkpoint head did not move: %#v", rec)
	}
}

func TestCheckpointRollbackKeepsParentMemComponentRecord(t *testing.T) {
	m, store, imageSvc, _, original := retireFixture(t)
	ctx := context.Background()

	registerErr := errors.New("register failed")
	if _, err := m.Checkpoint(ctx, "sandbox-a", func(context.Context, CheckpointResult) error {
		return registerErr
	}); !errors.Is(err, registerErr) {
		t.Fatalf("Checkpoint() error = %v, want %v", err, registerErr)
	}
	if got := imageSvc.deletedNames(); len(got) != 0 {
		t.Fatalf("rollback retired image records = %v, want none", got)
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.CheckpointHeadTemplateID != original.CheckpointHeadTemplateID {
		t.Fatalf("rollback head = %s, want %s", rec.CheckpointHeadTemplateID, original.CheckpointHeadTemplateID)
	}
}

// TestExpireRetiresMemComponentRecordBeforeCleanupFailure pins the failed
// expire's retire ordering: cleanup fails and persistDeleteFailure collapses
// the lineage in the same call, so the mem component record must already be
// retired when that write clears the head digest a retry would need.
func TestExpireRetiresMemComponentRecordBeforeCleanupFailure(t *testing.T) {
	m, store, imageSvc, memDesc, _ := retireFixture(t)
	ctx := context.Background()
	m.boot.(*recordingBootPreparer).releaseErr = errors.New("boot release failed")

	if err := m.Expire(ctx, "sandbox-a", time.Now()); err == nil {
		t.Fatal("expire with failing cleanup succeeded")
	}
	got, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateUnknown || got.CheckpointHeadTemplateID != got.SourceTemplateID {
		t.Fatalf("failure record = %#v", got)
	}
	want := conchimage.ComponentImageName(conchimage.KindMemSnapshot, memDesc.Digest)
	if names := imageSvc.deletedNames(); len(names) != 1 || names[0] != want {
		t.Fatalf("deleted image records = %v, want [%s]", names, want)
	}
}
