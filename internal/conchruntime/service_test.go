package conchruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

type fakeSandboxOps struct {
	req                sandbox.CreateRequest
	checkpointRequests []sandbox.CheckpointRequest
	checkpointResults  []sandbox.CheckpointResult
	checkpointErr      error
	createResult       sandbox.CreateResult
	deleteErr          error
}

type serializedDeleteOps struct {
	fakeSandboxOps
	firstEntered chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int32
}

func (f *serializedDeleteOps) Delete(sandbox.DeleteRequest) error {
	if f.calls.Add(1) == 1 {
		close(f.firstEntered)
		<-f.releaseFirst
		return nil
	}
	return errors.New("sandbox not found")
}

func (f *fakeSandboxOps) Create(req sandbox.CreateRequest) (sandbox.CreateResult, error) {
	f.req = req
	result := f.createResult
	if result.Namespace == "" {
		result.Namespace = req.Namespace
	}
	if result.SandboxID == "" {
		result.SandboxID = req.SandboxID
	}
	if result.IP == "" {
		result.IP = "192.0.2.10"
	}
	if result.AgentToken == "" {
		result.AgentToken = req.AgentToken
	}
	return result, nil
}

func (f *fakeSandboxOps) Delete(sandbox.DeleteRequest) error {
	return f.deleteErr
}

func (f *fakeSandboxOps) Suspend(sandbox.LifecycleRequest) error {
	return nil
}

func (f *fakeSandboxOps) Resume(sandbox.LifecycleRequest) error {
	return nil
}

func (f *fakeSandboxOps) Checkpoint(req sandbox.CheckpointRequest) (sandbox.CheckpointResult, error) {
	call := len(f.checkpointRequests)
	f.checkpointRequests = append(f.checkpointRequests, req)
	if f.checkpointErr != nil {
		return sandbox.CheckpointResult{}, f.checkpointErr
	}
	if call < len(f.checkpointResults) {
		return f.checkpointResults[call], nil
	}
	return sandbox.CheckpointResult{}, nil
}

func TestCheckpointSandboxPublishesCaptureAndAtomicallyAdvancesHead(t *testing.T) {
	ctx := context.Background()
	t0Digest := digest.FromString("checkpoint-t0").String()
	t1Digest := digest.FromString("checkpoint-t1").String()
	memRoot := t.TempDir()
	captured := sandbox.CapturedBootComponents{
		MemRootPath:  memRoot,
		VMMName:      "cloud-hypervisor",
		MemorySizeMB: 512,
	}
	sandboxOps := &fakeSandboxOps{checkpointResults: []sandbox.CheckpointResult{captured}}
	imageOps := &templateBuildImageOps{checkpointResults: []conchimage.PublishCheckpointBootImageResult{{
		BootIndexDigest: t1Digest,
		ImageName:       "localhost/conch/checkpoints:t1",
	}}}
	store := newTestStore(t)
	svc := New(sandboxOps, imageOps, imageOps, store, "default")
	seedTemplate(t, ctx, svc.Templates, "t0", "team-a", t0Digest, conchtemplate.BootModeCold)

	before := state.SandboxRecord{
		SandboxID:                     "sandbox-a",
		Namespace:                     "team-a",
		State:                         state.SandboxReady,
		SourceTemplateID:              "t0",
		SourceBootIndexDigest:         t0Digest,
		CheckpointHeadTemplateID:      "t0",
		CheckpointHeadBootIndexDigest: t0Digest,
		VMMName:                       "cloud-hypervisor",
		VMMPID:                        1234,
		VMMSocketPath:                 "/run/conch/sandbox-a/vmm.sock",
		VsockCID:                      17,
		VsockSocketPath:               "/run/conch/sandbox-a/vsock.sock",
		RootfsKey:                     "sandbox-a-rootfs",
		MemKey:                        "sandbox-a-mem",
		RootfsMount:                   "/run/conch/sandbox-a/rootfs",
		RootfsPmemPaths:               []string{"/run/conch/sandbox-a/rootfs/base.erofs"},
		MemMount:                      "/run/conch/sandbox-a/mem",
		VMMount:                       "/run/conch/sandbox-a/vm",
		SnapshotRootDir:               "conch/snapshot",
	}
	if err := store.UpsertSandbox(ctx, before); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	result, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{
		Namespace: "team-a",
		SandboxID: "sandbox-a",
		Labels:    map[string]string{"generation": "t1"},
	})
	if err != nil {
		t.Fatalf("CheckpointSandbox() error = %v", err)
	}
	if result.TemplateID == "" || result.BootIndexDigest != t1Digest {
		t.Fatalf("CheckpointSandbox() = %#v", result)
	}
	if len(sandboxOps.checkpointRequests) != 1 {
		t.Fatalf("checkpoint requests = %#v, want one", sandboxOps.checkpointRequests)
	}
	// Generation identity and parent snapshot IDs are deliberately absent from
	// the runtime capture seam; it receives only the sandbox identity.
	if got, want := sandboxOps.checkpointRequests[0], (sandbox.CheckpointRequest{
		Namespace: "team-a",
		SandboxID: "sandbox-a",
	}); got != want {
		t.Fatalf("checkpoint request = %#v, want %#v", got, want)
	}
	if len(imageOps.checkpointCalls) != 1 {
		t.Fatalf("checkpoint publish calls = %#v, want one", imageOps.checkpointCalls)
	}
	publish := imageOps.checkpointCalls[0]
	if publish.Namespace != "team-a" || publish.SourceBootIndexDigest != t0Digest {
		t.Fatalf("checkpoint publish source = %#v", publish)
	}
	if publish.BootIndexTag != "localhost/conch/template:"+result.TemplateID ||
		publish.MemRoot != memRoot || publish.VMMName != "cloud-hypervisor" || publish.MemorySizeMB != 512 {
		t.Fatalf("checkpoint publish capture = %#v, want %#v", publish, captured)
	}
	if _, err := os.Stat(memRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("captured memory root still exists after publication: %v", err)
	}

	t1, err := store.GetTemplate(ctx, result.TemplateID)
	if err != nil {
		t.Fatalf("GetTemplate(t1) error = %v", err)
	}
	if t1.BootIndexDigest != t1Digest || t1.BootMode != conchtemplate.BootModeResume {
		t.Fatalf("t1 entry = %#v", t1)
	}
	if t1.ParentTemplateID != "t0" || t1.SourceSandboxID != "sandbox-a" ||
		t1.BuildRef != "localhost/conch/checkpoints:t1" || t1.Labels["generation"] != "t1" {
		t.Fatalf("t1 lineage = %#v", t1)
	}

	after, err := store.GetSandbox(ctx, "sandbox-a")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	wantAfter := before
	wantAfter.CheckpointHeadTemplateID = result.TemplateID
	wantAfter.CheckpointHeadBootIndexDigest = t1Digest
	if !reflect.DeepEqual(after, wantAfter) {
		t.Fatalf("sandbox record after checkpoint = %#v, want only checkpoint head changed from %#v", after, before)
	}
}

func TestCheckpointSandboxDoesNotPersistBeforeValidationSucceeds(t *testing.T) {
	ctx := context.Background()
	sourceDigest := digest.FromString("checkpoint-source").String()
	checkpointDigest := digest.FromString("invalid-checkpoint").String()
	memRoot := t.TempDir()
	sandboxOps := &fakeSandboxOps{checkpointResults: []sandbox.CheckpointResult{{
		MemRootPath:  memRoot,
		VMMName:      "cloud-hypervisor",
		MemorySizeMB: 512,
	}}}
	imageOps := &templateBuildImageOps{
		checkpointResults: []conchimage.PublishCheckpointBootImageResult{{
			BootIndexDigest: checkpointDigest,
			ImageName:       "localhost/conch/checkpoints:invalid",
		}},
		inspectErr: errors.New("invalid checkpoint boot index"),
	}
	store := newTestStore(t)
	svc := New(sandboxOps, imageOps, imageOps, store, "default")
	seedTemplate(t, ctx, svc.Templates, "t0", "team-a", sourceDigest, conchtemplate.BootModeCold)
	before := state.SandboxRecord{
		SandboxID:                     "sandbox-validation-failure",
		Namespace:                     "team-a",
		State:                         state.SandboxReady,
		SourceTemplateID:              "t0",
		SourceBootIndexDigest:         sourceDigest,
		CheckpointHeadTemplateID:      "t0",
		CheckpointHeadBootIndexDigest: sourceDigest,
	}
	if err := store.UpsertSandbox(ctx, before); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	if _, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{
		Namespace: "team-a",
		SandboxID: before.SandboxID,
	}); err == nil {
		t.Fatal("CheckpointSandbox() error = nil, want validation failure")
	}
	templates, err := store.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates() error = %v", err)
	}
	if len(templates) != 1 || templates[0].ID != "t0" {
		t.Fatalf("templates after failed validation = %#v, want only source template", templates)
	}
	after, err := store.GetSandbox(ctx, before.SandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("sandbox changed after failed validation: got %#v, want %#v", after, before)
	}
}

func TestCheckpointSandboxBuildsConsecutiveTemplateLineage(t *testing.T) {
	ctx := context.Background()
	t0Digest := digest.FromString("lineage-t0").String()
	t1Digest := digest.FromString("lineage-t1").String()
	t2Digest := digest.FromString("lineage-t2").String()
	memRoot1 := t.TempDir()
	memRoot2 := t.TempDir()
	sandboxOps := &fakeSandboxOps{checkpointResults: []sandbox.CheckpointResult{
		{MemRootPath: memRoot1, VMMName: "stratovirt", MemorySizeMB: 256},
		{MemRootPath: memRoot2, VMMName: "stratovirt", MemorySizeMB: 256},
	}}
	imageOps := &templateBuildImageOps{checkpointResults: []conchimage.PublishCheckpointBootImageResult{
		{BootIndexDigest: t1Digest, ImageName: "localhost/conch/checkpoints:t1"},
		{BootIndexDigest: t2Digest, ImageName: "localhost/conch/checkpoints:t2"},
	}}
	store := newTestStore(t)
	svc := New(sandboxOps, imageOps, imageOps, store, "default")
	seedTemplate(t, ctx, svc.Templates, "t0", "team-a", t0Digest, conchtemplate.BootModeCold)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID:                     "sandbox-lineage",
		Namespace:                     "team-a",
		State:                         state.SandboxReady,
		SourceTemplateID:              "t0",
		SourceBootIndexDigest:         t0Digest,
		CheckpointHeadTemplateID:      "t0",
		CheckpointHeadBootIndexDigest: t0Digest,
		RootfsKey:                     "runtime-rootfs",
		MemKey:                        "runtime-mem",
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	t1Result, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{Namespace: "team-a", SandboxID: "sandbox-lineage"})
	if err != nil {
		t.Fatalf("CheckpointSandbox(t1) error = %v", err)
	}
	t2Result, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{Namespace: "team-a", SandboxID: "sandbox-lineage"})
	if err != nil {
		t.Fatalf("CheckpointSandbox(t2) error = %v", err)
	}
	if t1Result.TemplateID == "" || t2Result.TemplateID == "" || t1Result.TemplateID == t2Result.TemplateID {
		t.Fatalf("checkpoint template ids = (%q, %q)", t1Result.TemplateID, t2Result.TemplateID)
	}
	if t1Result.BootIndexDigest != t1Digest || t2Result.BootIndexDigest != t2Digest {
		t.Fatalf("checkpoint digests = (%q, %q)", t1Result.BootIndexDigest, t2Result.BootIndexDigest)
	}

	if len(imageOps.checkpointCalls) != 2 {
		t.Fatalf("checkpoint publish calls = %#v, want two", imageOps.checkpointCalls)
	}
	if imageOps.checkpointCalls[0].SourceBootIndexDigest != t0Digest ||
		imageOps.checkpointCalls[1].SourceBootIndexDigest != t1Digest {
		t.Fatalf("checkpoint publish parents = (%q, %q), want (%q, %q)",
			imageOps.checkpointCalls[0].SourceBootIndexDigest,
			imageOps.checkpointCalls[1].SourceBootIndexDigest,
			t0Digest, t1Digest)
	}
	if imageOps.checkpointCalls[0].BootIndexTag != "localhost/conch/template:"+t1Result.TemplateID ||
		imageOps.checkpointCalls[1].BootIndexTag != "localhost/conch/template:"+t2Result.TemplateID {
		t.Fatalf("checkpoint publish tags = (%q, %q)", imageOps.checkpointCalls[0].BootIndexTag, imageOps.checkpointCalls[1].BootIndexTag)
	}
	if imageOps.checkpointCalls[0].MemorySizeMB != 256 || imageOps.checkpointCalls[1].MemorySizeMB != 256 {
		t.Fatalf("checkpoint memory sizes = (%d, %d)", imageOps.checkpointCalls[0].MemorySizeMB, imageOps.checkpointCalls[1].MemorySizeMB)
	}

	t1, err := store.GetTemplate(ctx, t1Result.TemplateID)
	if err != nil {
		t.Fatalf("GetTemplate(t1) error = %v", err)
	}
	t2, err := store.GetTemplate(ctx, t2Result.TemplateID)
	if err != nil {
		t.Fatalf("GetTemplate(t2) error = %v", err)
	}
	if t1.ParentTemplateID != "t0" || t2.ParentTemplateID != t1.ID {
		t.Fatalf("template lineage: t1 parent = %q, t2 parent = %q", t1.ParentTemplateID, t2.ParentTemplateID)
	}
	if t1.BootIndexDigest != t1Digest || t2.BootIndexDigest != t2Digest {
		t.Fatalf("checkpoint template entries = (%#v, %#v)", t1, t2)
	}

	rec, err := store.GetSandbox(ctx, "sandbox-lineage")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec.SourceTemplateID != "t0" || rec.SourceBootIndexDigest != t0Digest {
		t.Fatalf("sandbox immutable source changed: %#v", rec)
	}
	if rec.CheckpointHeadTemplateID != t2.ID || rec.CheckpointHeadBootIndexDigest != t2Digest {
		t.Fatalf("sandbox checkpoint head = (%q, %q), want (%q, %q)",
			rec.CheckpointHeadTemplateID, rec.CheckpointHeadBootIndexDigest, t2.ID, t2Digest)
	}
	if rec.RootfsKey != "runtime-rootfs" || rec.MemKey != "runtime-mem" {
		t.Fatalf("sandbox runtime snapshot fields changed: %#v", rec)
	}
}

func TestRemoveSandboxKeepsStateWhenCleanupFails(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID: "sandbox-1",
		Namespace: "default",
		State:     state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("cleanup failed")}
	svc := New(sandboxOps, nil, nil, store, "default")
	if err := svc.RemoveSandbox(ctx, "default", "sandbox-1"); err == nil {
		t.Fatalf("RemoveSandbox() error = nil, want cleanup error")
	}
	rec, err := store.GetSandbox(ctx, "sandbox-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec.State != state.SandboxUnknown {
		t.Fatalf("sandbox.State = %q, want %q", rec.State, state.SandboxUnknown)
	}
}

func TestRemoveSandboxDoesNotCreateStateForUnknownRuntime(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("sandbox not found")}
	svc := New(sandboxOps, nil, nil, store, "default")
	if err := svc.RemoveSandbox(ctx, "default", "missing-sandbox"); err != nil {
		t.Fatalf("RemoveSandbox() error = %v", err)
	}

	records, err := store.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("ListSandboxes() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("sandboxes = %#v, want none", records)
	}
}

func TestConcurrentRemoveSandboxCallsAreSerialized(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	rec := state.SandboxRecord{
		SandboxID: "sandbox-serialized",
		Namespace: "default",
		State:     state.SandboxReady,
	}
	if err := store.UpsertSandbox(ctx, rec); err != nil {
		t.Fatal(err)
	}
	ops := &serializedDeleteOps{
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	svc := New(ops, nil, nil, store, "default")

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- svc.RemoveSandbox(ctx, "default", rec.SandboxID) }()
	<-ops.firstEntered
	go func() { secondDone <- svc.RemoveSandbox(ctx, "default", rec.SandboxID) }()
	select {
	case err := <-secondDone:
		t.Fatalf("second RemoveSandbox returned before first completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(ops.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first RemoveSandbox() error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second RemoveSandbox() error = %v", err)
	}
	if got := ops.calls.Load(); got != 2 {
		t.Fatalf("Delete() calls = %d, want 2 serialized calls", got)
	}
	if _, err := store.GetSandbox(ctx, rec.SandboxID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("sandbox record remains after Remove: %v", err)
	}
}

func TestCreateSandboxStoresRuntimeFieldsOnSandboxRecord(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	sandboxOps := &fakeSandboxOps{
		createResult: sandbox.CreateResult{
			RootfsKey:   "sandbox-1",
			MemKey:      "sandbox-1-mem",
			RootfsMount: "/run/conch/rootfs",
			MemMount:    "/run/conch/mem",
			VMMount:     "/run/conch/vm",
			RootDir:     "conch/snapshot",
			MemSize:     512,
		},
	}
	svc := New(sandboxOps, nil, nil, store, "default")

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID:  "sandbox-1",
		TemplateID: "tmpl-1",
	}); err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	rec, err := store.GetSandbox(ctx, "sandbox-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec.RootfsKey != "sandbox-1" || rec.MemKey != "sandbox-1-mem" {
		t.Fatalf("runtime keys = (%q, %q), want sandbox keys", rec.RootfsKey, rec.MemKey)
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
	svc := New(sandboxOps, nil, nil, nil, "default")
	svc.SetSandboxDefaults(SandboxDefaults{
		TemplateID: "tmpl_default",
		VMMName:    "cloud-hypervisor",
		VCPUNum:    2,
		VCPUMax:    4,
		RamMB:      4096,
	})

	result, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
		SandboxID: "sandbox-1",
		Env:       map[string]string{"SOME_RANDOM_KEY": "key123"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	if sandboxOps.req.TemplateID != "tmpl_default" {
		t.Fatalf("TemplateID = %q", sandboxOps.req.TemplateID)
	}
	if sandboxOps.req.VMMName != "cloud-hypervisor" {
		t.Fatalf("VmmName = %q", sandboxOps.req.VMMName)
	}
	if sandboxOps.req.VCPUNum != 2 || sandboxOps.req.VCPUMax != 4 || sandboxOps.req.RAMMB != 4096 {
		t.Fatalf("resources = vcpu:%d max:%d ram:%d", sandboxOps.req.VCPUNum, sandboxOps.req.VCPUMax, sandboxOps.req.RAMMB)
	}
	if sandboxOps.req.AgentToken == "" {
		t.Fatal("AgentToken is empty")
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

func TestCreateSandboxRejectsMissingOrWhitespaceDefaultTemplate(t *testing.T) {
	for _, defaultTemplate := range []string{"", " \t "} {
		t.Run(fmt.Sprintf("default %q", defaultTemplate), func(t *testing.T) {
			sandboxOps := &fakeSandboxOps{}
			svc := New(sandboxOps, nil, nil, nil, "default")
			svc.SetSandboxDefaults(SandboxDefaults{TemplateID: defaultTemplate})

			_, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{TemplateID: " \n "})
			if !errors.Is(err, ErrTemplateIDRequired) {
				t.Fatalf("CreateSandbox() error = %v, want ErrTemplateIDRequired", err)
			}
			if sandboxOps.req.TemplateID != "" {
				t.Fatalf("sandbox create was called with %#v", sandboxOps.req)
			}
		})
	}
}

func TestCreateSandboxGeneratesSingleSandboxID(t *testing.T) {
	store := newTestStore(t)
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil, store, "default")
	svc.SetSandboxDefaults(SandboxDefaults{TemplateID: "tmpl-default"})

	result, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	if result.SandboxID == "" || sandboxOps.req.SandboxID != result.SandboxID {
		t.Fatalf("sandbox identity = result:%q request:%q", result.SandboxID, sandboxOps.req.SandboxID)
	}
	record, err := store.GetSandbox(context.Background(), result.SandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if record.SandboxID != result.SandboxID {
		t.Fatalf("record.SandboxID = %q, want %q", record.SandboxID, result.SandboxID)
	}
}

func TestCreateSandboxKeepsExplicitOptions(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil, nil, "default")
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

	if sandboxOps.req.TemplateID != "tmpl_resume_explicit" || sandboxOps.req.VMMName != "explicit-vmm" {
		t.Fatalf("request = %#v", sandboxOps.req)
	}
	if sandboxOps.req.VCPUNum != 6 || sandboxOps.req.VCPUMax != 8 || sandboxOps.req.RAMMB != 8192 {
		t.Fatalf("resources = vcpu:%d max:%d ram:%d", sandboxOps.req.VCPUNum, sandboxOps.req.VCPUMax, sandboxOps.req.RAMMB)
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

func seedTemplate(
	t *testing.T,
	ctx context.Context,
	templates conchtemplate.Store,
	id string,
	namespace string,
	bootIndexDigest string,
	bootMode conchtemplate.BootMode,
) {
	t.Helper()
	if _, err := templates.Create(ctx, conchtemplate.Entry{
		ID:              id,
		Origin:          conchtemplate.OriginImage,
		BootMode:        bootMode,
		BootIndexDigest: bootIndexDigest,
		Namespace:       namespace,
		BuildRef:        "localhost/conch/templates:" + id,
	}); err != nil {
		t.Fatalf("CreateTemplate(%s) error = %v", id, err)
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
