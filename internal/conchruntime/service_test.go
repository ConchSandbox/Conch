package conchruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	containerderrdefs "github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/internal/apperror"
	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/id"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/internal/webhook"
)

type fakeSandboxOps struct {
	req                sandbox.CreateRequest
	checkpointRequests []sandbox.CheckpointRequest
	checkpointResults  []sandbox.CheckpointResult
	checkpointErr      error
	createResult       sandbox.CreateResult
	createErr          error
	createCalls        int
	deleteErr          error
	updateReq          sandbox.NetworkUpdateRequest
	updateErr          error
	createHook         func()
}

type fakeTemplateStore struct {
	entries map[string]conchtemplate.Entry
}

const testTemplateName = "registry.example/conch/test:latest"

func (f *fakeTemplateStore) Put(_ context.Context, entry conchtemplate.Entry, _ ocispec.Descriptor) (conchtemplate.Entry, error) {
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

func TestSandboxLifecycleEventsPublishedAfterCreateAndDelete(t *testing.T) {
	store, err := state.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	events := make(chan webhook.Event, 2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event webhook.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode event: %v", err)
			return
		}
		events <- event
	}))
	defer receiver.Close()
	dispatcher := webhook.NewDispatcher()
	if _, err := dispatcher.Create(runtimeapi.WebhookCreateOptions{Name: "receiver", URL: receiver.URL}); err != nil {
		t.Fatalf("register webhook: %v", err)
	}
	templateID := digest.FromString("lifecycle-event-template").String()
	const templateName = "registry.example/conch/lifecycle:latest"
	svc := New(&fakeSandboxOps{createResult: sandbox.CreateResult{BootIndexDigest: templateID}}, nil, store)
	setFakeTemplate(svc, templateName, templateID, conchtemplate.BootModeCold)
	svc.WebhookDispatcher = dispatcher
	created, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{SandboxID: "sandbox-events", TemplateName: templateName, VCPUNum: 2, VCPUMax: 2, RamMB: 512})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	select {
	case event := <-events:
		if event.Type != webhook.EventSandboxCreated || event.EventData.KillReason != "" || event.SandboxID != created.SandboxID || event.EventData.Execution.VCPUNum != 2 || event.EventData.Execution.RamMB != 512 {
			t.Fatalf("created event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("created event not delivered")
	}
	if err := svc.RemoveSandbox(context.Background(), created.SandboxID); err != nil {
		t.Fatalf("RemoveSandbox: %v", err)
	}
	select {
	case event := <-events:
		if event.Type != webhook.EventSandboxKilled || event.EventData.KillReason != "request" || event.SandboxID != created.SandboxID {
			t.Fatalf("killed event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("killed event not delivered")
	}
}

func TestHandleSandboxUnexpectedExitMarksUnknownAndPublishesOnce(t *testing.T) {
	store, err := state.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	templateID := digest.FromString("orphaned-event-template").String()
	record := state.SandboxRecord{SandboxID: "sandbox-orphaned", State: state.SandboxReady, CreatedAt: time.Now().UnixNano(), CheckpointHeadTemplateID: templateID, VCPUNum: 2, RamMB: 512}
	if err := store.UpsertSandbox(context.Background(), record); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	events := make(chan webhook.Event, 2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event webhook.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err == nil {
			events <- event
		}
	}))
	defer receiver.Close()
	dispatcher := webhook.NewDispatcher()
	if _, err := dispatcher.Create(runtimeapi.WebhookCreateOptions{Name: "receiver", URL: receiver.URL}); err != nil {
		t.Fatalf("register webhook: %v", err)
	}
	svc := New(nil, nil, store)
	svc.WebhookDispatcher = dispatcher
	svc.HandleSandboxUnexpectedExit(record.SandboxID)
	svc.HandleSandboxUnexpectedExit(record.SandboxID)
	select {
	case event := <-events:
		if event.Type != webhook.EventSandboxKilled || event.EventData.KillReason != "orphaned" || event.SandboxID != record.SandboxID {
			t.Fatalf("orphaned event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("orphaned event not delivered")
	}
	select {
	case event := <-events:
		t.Fatalf("unexpected duplicate orphaned event = %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
	updated, err := store.GetSandbox(context.Background(), record.SandboxID)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if updated.State != state.SandboxUnknown {
		t.Fatalf("state = %q, want %q", updated.State, state.SandboxUnknown)
	}
}

type serializedDeleteOps struct {
	fakeSandboxOps
	firstEntered chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int32
}

type failingCheckpointStore struct {
	state.Store
}

func (failingCheckpointStore) AdvanceCheckpointHead(context.Context, string, string, string) error {
	return errors.New("checkpoint head changed")
}

func (f *serializedDeleteOps) Delete(sandbox.DeleteRequest) error {
	if f.calls.Add(1) == 1 {
		close(f.firstEntered)
		<-f.releaseFirst
		return nil
	}
	return sandbox.ErrNotFound.New()
}

func (f *fakeSandboxOps) Create(req sandbox.CreateRequest) (sandbox.CreateResult, error) {
	f.createCalls++
	f.req = req
	if f.createHook != nil {
		f.createHook()
	}
	result := f.createResult
	if result.SandboxID == "" {
		result.SandboxID = req.SandboxID
	}
	if result.IP == "" {
		result.IP = "192.0.2.10"
	}
	if result.AgentToken == "" {
		result.AgentToken = req.AgentToken
	}
	return result, f.createErr
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

func (f *fakeSandboxOps) UpdateNetwork(_ context.Context, req sandbox.NetworkUpdateRequest) error {
	f.updateReq = req
	return f.updateErr
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

func TestCombineOperationErrorsPreservesPrimaryClassification(t *testing.T) {
	internalPrimary := errors.New("state write failed")
	combined := combineOperationErrors(internalPrimary, sandbox.ErrNotFound.New())
	var appErr *apperror.Error
	if errors.As(combined, &appErr) {
		t.Fatalf("secondary application error changed internal primary: %#v", appErr)
	}
	if !errors.Is(combined, internalPrimary) {
		t.Fatal("primary cause was not retained")
	}

	classifiedPrimary := sandbox.ErrFailedPrecondition.Wrap(errors.New("state changed"))
	combined = combineOperationErrors(classifiedPrimary, sandbox.ErrNotFound.New())
	if !errors.As(combined, &appErr) || appErr.Code() != sandbox.ErrFailedPrecondition.Code() {
		t.Fatalf("classification = %#v, want %s", appErr, sandbox.ErrFailedPrecondition.Code())
	}
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

func TestCheckpointSandboxPublishesCaptureAndAtomicallyAdvancesHead(t *testing.T) {
	ctx := context.Background()
	host := newRuntimeImageHost(t)
	t0Digest := buildColdBootIndex(t, host, "checkpoint-t0")
	const sourceName = "registry.example/conch/checkpoint-source:latest"
	const checkpointName = "registry.example/conch/checkpoint-result:latest"
	memRoot := t.TempDir()
	captured := sandbox.CapturedBootComponents{
		MemRootPath:  memRoot,
		VMMName:      "cloud-hypervisor",
		MemorySizeMB: 512,
	}
	sandboxOps := &fakeSandboxOps{checkpointResults: []sandbox.CheckpointResult{captured}}
	store := newTestStore(t)
	svc := New(sandboxOps, host.Client(), store)
	svc.Templates = host.TemplateStore()
	seedTemplate(t, ctx, host, sourceName, t0Digest, conchtemplate.BootModeCold)

	before := state.SandboxRecord{
		SandboxID:                "sandbox-a",
		CheckpointHeadTemplateID: t0Digest,
	}
	if err := store.UpsertSandbox(ctx, before); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	result, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{
		SandboxID:    "sandbox-a",
		TemplateName: checkpointName,
		Labels:       map[string]string{"generation": "t1"},
	})
	if err != nil {
		t.Fatalf("CheckpointSandbox() error = %v", err)
	}
	if result.TemplateID == "" || result.TemplateID == t0Digest {
		t.Fatalf("CheckpointSandbox() = %#v", result)
	}
	if len(sandboxOps.checkpointRequests) != 1 {
		t.Fatalf("checkpoint requests = %#v, want one", sandboxOps.checkpointRequests)
	}
	// Generation identity and parent snapshot IDs are deliberately absent from
	// the runtime capture seam; it receives only the sandbox identity.
	if got, want := sandboxOps.checkpointRequests[0], (sandbox.CheckpointRequest{
		SandboxID: "sandbox-a",
	}); got != want {
		t.Fatalf("checkpoint request = %#v, want %#v", got, want)
	}
	if _, err := os.Stat(memRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("captured memory root still exists after publication: %v", err)
	}

	t1, err := svc.Templates.Get(ctx, checkpointName)
	if err != nil {
		t.Fatalf("GetTemplate(t1) error = %v", err)
	}
	if t1.BootIndexDigest != result.TemplateID || t1.BootMode != conchtemplate.BootModeResume {
		t.Fatalf("t1 entry = %#v", t1)
	}
	if t1.ParentBootIndexDigest != t0Digest || t1.SourceSandboxID != "sandbox-a" || t1.Labels["generation"] != "t1" {
		t.Fatalf("t1 lineage = %#v", t1)
	}

	after, err := store.GetSandbox(ctx, "sandbox-a")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	wantAfter := before
	wantAfter.CheckpointHeadTemplateID = result.TemplateID
	if !reflect.DeepEqual(after, wantAfter) {
		t.Fatalf("sandbox record after checkpoint = %#v, want only checkpoint head changed from %#v", after, before)
	}
}

func TestCheckpointSandboxDoesNotPersistBeforeValidationSucceeds(t *testing.T) {
	ctx := context.Background()
	host := newRuntimeImageHost(t)
	sourceDigest := buildColdBootIndex(t, host, "checkpoint-source")
	const sourceName = "registry.example/conch/validation-source:latest"
	const checkpointName = "registry.example/conch/validation-result:latest"
	memRoot := t.TempDir()
	sandboxOps := &fakeSandboxOps{checkpointResults: []sandbox.CheckpointResult{{
		MemRootPath:  memRoot,
		VMMName:      "cloud-hypervisor",
		MemorySizeMB: 512,
	}}}
	store := newTestStore(t)
	svc := New(sandboxOps, host.Client(), store)
	svc.Templates = host.TemplateStore()
	seedTemplate(t, ctx, host, sourceName, sourceDigest, conchtemplate.BootModeCold)
	if err := host.Client().ContentStore().Delete(
		containerdclient.NewNamespaceContext(ctx), digest.Digest(sourceDigest),
	); err != nil {
		t.Fatalf("delete source Boot Index content: %v", err)
	}
	before := state.SandboxRecord{
		SandboxID:                "sandbox-validation-failure",
		CheckpointHeadTemplateID: sourceDigest,
	}
	if err := store.UpsertSandbox(ctx, before); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	if _, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{
		SandboxID:    before.SandboxID,
		TemplateName: checkpointName,
	}); err == nil {
		t.Fatal("CheckpointSandbox() error = nil, want validation failure")
	}
	records, err := host.Client().ImageService().List(containerdclient.NewNamespaceContext(ctx))
	if err != nil {
		t.Fatalf("List image records: %v", err)
	}
	if len(sandboxOps.checkpointRequests) != 0 {
		t.Fatalf("checkpoint capture ran after failed preflight: %#v", sandboxOps.checkpointRequests)
	}
	for _, record := range records {
		if record.Name == conchimage.TemplateRecordName(checkpointName) {
			t.Fatalf("checkpoint name was created after failed validation: %#v", record)
		}
	}
	after, err := store.GetSandbox(ctx, before.SandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("sandbox changed after failed validation: got %#v, want %#v", after, before)
	}
}

func TestCheckpointSandboxRemovesTemplateWhenHeadAdvanceFails(t *testing.T) {
	ctx := context.Background()
	host := newRuntimeImageHost(t)
	sourceDigest := buildColdBootIndex(t, host, "checkpoint-cas-source")
	const sourceName = "registry.example/conch/cas-source:latest"
	const checkpointName = "registry.example/conch/cas-result:latest"
	store := newTestStore(t)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID:                "sandbox-cas",
		CheckpointHeadTemplateID: sourceDigest,
	}); err != nil {
		t.Fatal(err)
	}
	seedTemplate(t, ctx, host, sourceName, sourceDigest, conchtemplate.BootModeCold)
	svc := New(&fakeSandboxOps{checkpointResults: []sandbox.CheckpointResult{{
		MemRootPath:  t.TempDir(),
		VMMName:      "cloud-hypervisor",
		MemorySizeMB: 128,
	}}}, host.Client(), failingCheckpointStore{Store: store})
	svc.Templates = host.TemplateStore()

	if _, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{SandboxID: "sandbox-cas", TemplateName: checkpointName}); err == nil {
		t.Fatal("CheckpointSandbox() error = nil, want checkpoint head failure")
	}
	records, err := host.Client().ImageService().List(containerdclient.NewNamespaceContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Name == conchimage.TemplateRecordName(checkpointName) {
			t.Fatalf("checkpoint name exists after CAS failure: %#v", record)
		}
	}
}

func TestCheckpointSandboxBuildsConsecutiveTemplateLineage(t *testing.T) {
	ctx := context.Background()
	host := newRuntimeImageHost(t)
	t0Digest := buildColdBootIndex(t, host, "lineage-t0")
	const sourceName = "registry.example/conch/lineage-source:latest"
	const t1Name = "registry.example/conch/lineage-t1:latest"
	const t2Name = "registry.example/conch/lineage-t2:latest"
	memRoot1 := t.TempDir()
	memRoot2 := t.TempDir()
	sandboxOps := &fakeSandboxOps{checkpointResults: []sandbox.CheckpointResult{
		{MemRootPath: memRoot1, VMMName: "stratovirt", MemorySizeMB: 256},
		{MemRootPath: memRoot2, VMMName: "stratovirt", MemorySizeMB: 256},
	}}
	store := newTestStore(t)
	svc := New(sandboxOps, host.Client(), store)
	svc.Templates = host.TemplateStore()
	seedTemplate(t, ctx, host, sourceName, t0Digest, conchtemplate.BootModeCold)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID:                "sandbox-lineage",
		CheckpointHeadTemplateID: t0Digest,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	t1Result, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{SandboxID: "sandbox-lineage", TemplateName: t1Name})
	if err != nil {
		t.Fatalf("CheckpointSandbox(t1) error = %v", err)
	}
	t2Result, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{SandboxID: "sandbox-lineage", TemplateName: t2Name})
	if err != nil {
		t.Fatalf("CheckpointSandbox(t2) error = %v", err)
	}
	if t1Result.TemplateID == "" || t2Result.TemplateID == "" || t1Result.TemplateID == t2Result.TemplateID {
		t.Fatalf("checkpoint digests = (%q, %q)", t1Result.TemplateID, t2Result.TemplateID)
	}

	t1, err := svc.Templates.Get(ctx, t1Name)
	if err != nil {
		t.Fatalf("GetTemplate(t1) error = %v", err)
	}
	t2, err := svc.Templates.Get(ctx, t2Name)
	if err != nil {
		t.Fatalf("GetTemplate(t2) error = %v", err)
	}
	if t1.ParentBootIndexDigest != t0Digest || t2.ParentBootIndexDigest != t1.BootIndexDigest {
		t.Fatalf("template lineage: t1 parent = %q, t2 parent = %q", t1.ParentBootIndexDigest, t2.ParentBootIndexDigest)
	}
	if t1.BootIndexDigest != t1Result.TemplateID || t2.BootIndexDigest != t2Result.TemplateID {
		t.Fatalf("checkpoint template entries = (%#v, %#v)", t1, t2)
	}

	rec, err := store.GetSandbox(ctx, "sandbox-lineage")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec.CheckpointHeadTemplateID != t2Result.TemplateID {
		t.Fatalf("sandbox checkpoint head = %q, want %q", rec.CheckpointHeadTemplateID, t2Result.TemplateID)
	}

	if err := svc.RemoveTemplate(ctx, t1Name); err != nil {
		t.Fatalf("RemoveTemplate(t1) error = %v", err)
	}
	if err := svc.RemoveTemplate(ctx, t1Name); !errors.Is(err, conchtemplate.ErrNotFound) {
		t.Fatalf("RemoveTemplate(t1) second call error = %v, want ErrNotFound", err)
	}
	if _, err := svc.Templates.Get(ctx, t1Name); !errors.Is(err, conchtemplate.ErrNotFound) {
		t.Fatalf("GetTemplate(t1) after removal error = %v, want ErrNotFound", err)
	}
	imageCtx := containerdclient.NewNamespaceContext(ctx)
	if _, err := host.Client().ImageService().Get(imageCtx, conchimage.TemplateRecordName(t1Name)); !containerderrdefs.IsNotFound(err) {
		t.Fatalf("named t1 image after removal error = %v, want not found", err)
	}
	if _, err := conchimage.InspectBootIndex(ctx, host.Client(), t2Result.TemplateID); err != nil {
		t.Fatalf("InspectBootIndex(t2) after removing shared-component t1: %v", err)
	}
}

func TestRemoveSandboxKeepsRecordWhenCleanupFails(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	before := state.SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: digest.FromString("sandbox-1-head").String(),
	}
	if err := store.UpsertSandbox(ctx, before); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("cleanup failed")}
	svc := New(sandboxOps, nil, store)
	if err := svc.RemoveSandbox(ctx, "sandbox-1"); err == nil {
		t.Fatalf("RemoveSandbox() error = nil, want cleanup error")
	}
	rec, err := store.GetSandbox(ctx, "sandbox-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec != before {
		t.Fatalf("sandbox record = %#v, want unchanged %#v", rec, before)
	}
}

func TestRemoveSandboxDoesNotCreateStateForUnknownRuntime(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	sandboxOps := &fakeSandboxOps{deleteErr: sandbox.ErrNotFound.New()}
	svc := New(sandboxOps, nil, store)
	if err := svc.RemoveSandbox(ctx, "missing-sandbox"); err != nil {
		t.Fatalf("RemoveSandbox() error = %v", err)
	}

	if _, err := store.GetSandbox(ctx, "missing-sandbox"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("GetSandbox() error = %v, want ErrNotFound", err)
	}
}

func TestConcurrentRemoveSandboxCallsAreSerialized(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	rec := state.SandboxRecord{
		SandboxID:                "sandbox-serialized",
		CheckpointHeadTemplateID: digest.FromString("sandbox-serialized-head").String(),
	}
	if err := store.UpsertSandbox(ctx, rec); err != nil {
		t.Fatal(err)
	}
	ops := &serializedDeleteOps{
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	svc := New(ops, nil, store)

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- svc.RemoveSandbox(ctx, rec.SandboxID) }()
	<-ops.firstEntered
	go func() { secondDone <- svc.RemoveSandbox(ctx, rec.SandboxID) }()
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

func TestCreateSandboxStoresAPIAndCheckpointMetadata(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	templateID := digest.FromString("sandbox-1-template").String()

	sandboxOps := &fakeSandboxOps{
		createResult: sandbox.CreateResult{
			BootIndexDigest: templateID,
		},
	}
	svc := New(sandboxOps, nil, store)
	setFakeTemplate(svc, testTemplateName, templateID, conchtemplate.BootModeCold)

	result, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID:    "sandbox-1",
		TemplateName: testTemplateName,
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	rec, err := store.GetSandbox(ctx, "sandbox-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	want := state.SandboxRecord{
		SandboxID:                "sandbox-1",
		State:                    state.SandboxReady,
		CreatedAt:                result.CreatedAt,
		SourceTemplateName:       testTemplateName,
		SourceTemplateID:         templateID,
		CheckpointHeadTemplateID: templateID,
		IP:                       result.IP,
		VCPUNum:                  2,
		RamMB:                    1024,
	}
	if rec != want {
		t.Fatalf("sandbox record = %#v, want %#v", rec, want)
	}
}

func TestCreateSandboxReadyStateFailureDeletesCreatingRecordWhenVMMExited(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("delete API failed after VMM exited")}
	svc := New(sandboxOps, nil, store)
	templateID := digest.FromString("sandbox-1-source").String()
	setFakeTemplate(svc, testTemplateName, templateID, conchtemplate.BootModeCold)

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID:    "sandbox-1",
		TemplateName: testTemplateName,
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
	}); err == nil {
		t.Fatal("CreateSandbox() error = nil, want READY state persistence failure")
	}
	if _, err := store.GetSandbox(ctx, "sandbox-1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("GetSandbox() error = %v, want ErrNotFound", err)
	}
}

func TestCreateSandboxReadyStateFailureDeletesCreatingRecordWhenVMMExitIsUnconfirmed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("VMM process exit could not be confirmed")}
	svc := New(sandboxOps, nil, store)
	setFakeTemplate(svc, testTemplateName, digest.FromString("template-1").String(), conchtemplate.BootModeCold)

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID:    "sandbox-1",
		TemplateName: testTemplateName,
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
	}); err == nil {
		t.Fatal("CreateSandbox() error = nil, want READY state persistence failure")
	}
	if _, err := store.GetSandbox(ctx, "sandbox-1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("GetSandbox() error = %v, want ErrNotFound", err)
	}
}

func TestCreateSandboxFailureDeletesCreatingRecordAfterSuccessfulCleanup(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	svc := New(&fakeSandboxOps{createErr: errors.New("create failed")}, nil, store)
	setFakeTemplate(svc, testTemplateName, digest.FromString("template-1").String(), conchtemplate.BootModeCold)

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID:    "sandbox-1",
		TemplateName: testTemplateName,
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
	}); err == nil {
		t.Fatal("CreateSandbox() error = nil, want create failure")
	}
	if _, err := store.GetSandbox(ctx, "sandbox-1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("GetSandbox() error = %v, want ErrNotFound", err)
	}
}

func TestCreateSandboxFailureDeletesCreatingRecordWhenCleanupFails(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	createErr := errors.Join(errors.New("create failed"), errors.New("VMM cleanup failed"))
	svc := New(&fakeSandboxOps{createErr: createErr}, nil, store)
	setFakeTemplate(svc, testTemplateName, digest.FromString("template-1").String(), conchtemplate.BootModeCold)

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID:    "sandbox-1",
		TemplateName: testTemplateName,
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
	}); err == nil {
		t.Fatal("CreateSandbox() error = nil, want create failure")
	}
	if _, err := store.GetSandbox(ctx, "sandbox-1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("GetSandbox() error = %v, want ErrNotFound", err)
	}
}

func TestCreateSandboxPersistsCreatingRecordBeforeRuntimeCreate(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	ops := &fakeSandboxOps{createErr: errors.New("create failed")}
	ops.createHook = func() {
		rec, err := store.GetSandbox(ctx, "sandbox-1")
		if err != nil {
			t.Fatalf("GetSandbox() during Create: %v", err)
		}
		if rec.State != state.SandboxCreating || rec.VMMPID != 0 {
			t.Fatalf("creating record = %#v, want CREATING without PID", rec)
		}
	}
	svc := New(ops, nil, store)
	setFakeTemplate(svc, testTemplateName, digest.FromString("template-1").String(), conchtemplate.BootModeCold)

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID:    "sandbox-1",
		TemplateName: testTemplateName,
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
	}); err == nil {
		t.Fatal("CreateSandbox() error = nil, want create failure")
	}
	if _, err := store.GetSandbox(ctx, "sandbox-1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("GetSandbox() after failed create error = %v, want ErrNotFound", err)
	}
}

func TestCreateSandboxRejectsInvalidEnvironmentBeforeRuntimeCreate(t *testing.T) {
	for _, env := range []map[string]string{
		{"BAD=KEY": "value"},
		{"KEY": "bad\x00value"},
	} {
		ops := &fakeSandboxOps{}
		svc := New(ops, nil, nil)

		_, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
			SandboxID:    "sandbox-1",
			TemplateName: testTemplateName,
			VCPUNum:      2,
			VCPUMax:      2,
			RamMB:        1024,
			Env:          env,
		})
		if !errors.Is(err, agentprotocol.ErrInvalidEnvironment) {
			t.Fatalf("CreateSandbox(%q) error = %v, want ErrInvalidEnvironment", env, err)
		}
		if ops.createCalls != 0 {
			t.Fatalf("CreateSandbox(%q) runtime Create() calls = %d, want 0", env, ops.createCalls)
		}
	}
}

func TestCreateSandboxValidatesExplicitSandboxID(t *testing.T) {
	tests := []struct {
		name      string
		sandboxID string
		wantErr   bool
		wantText  string
	}{
		{name: "letters and separators", sandboxID: "sandbox.V1_test-01"},
		{name: "maximum length", sandboxID: strings.Repeat("a", 32)},
		{name: "too long", sandboxID: strings.Repeat("a", 33), wantErr: true, wantText: "length must be between 2 and 32"},
		{name: "command substitution", sandboxID: "x$(sleep${IFS}5)", wantErr: true, wantText: "only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed"},
		{name: "shell separator", sandboxID: "x;id", wantErr: true},
		{name: "path separator", sandboxID: "x/y", wantErr: true},
		{name: "embedded whitespace", sandboxID: "x y", wantErr: true},
		{name: "newline", sandboxID: "x\ny", wantErr: true},
		{name: "leading separator", sandboxID: "-sandbox", wantErr: true},
		{name: "single character", sandboxID: "a", wantErr: true, wantText: "length must be between 2 and 32"},
		{name: "non ASCII", sandboxID: "sandbox-\u6d4b\u8bd5", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeSandboxOps{}
			svc := New(ops, nil, nil)
			svc.SetSandboxDefaults(SandboxDefaults{VCPUNum: 2, VCPUMax: 2, RamMB: 1024})
			setFakeTemplate(svc, testTemplateName, digest.FromString("template-1").String(), conchtemplate.BootModeCold)

			result, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
				SandboxID:    tt.sandboxID,
				TemplateName: testTemplateName,
			})
			if tt.wantErr {
				if !errors.Is(err, sandbox.ErrInvalidArgument) {
					t.Fatalf("CreateSandbox() error = %v, want sandbox.ErrInvalidArgument", err)
				}
				if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
					t.Fatalf("CreateSandbox() error = %q, want text %q", err, tt.wantText)
				}
				if ops.createCalls != 0 {
					t.Fatalf("runtime Create() calls = %d, want 0", ops.createCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateSandbox() error = %v", err)
			}
			if result.SandboxID != tt.sandboxID || ops.req.SandboxID != tt.sandboxID {
				t.Fatalf("sandbox ID = result:%q request:%q, want %q", result.SandboxID, ops.req.SandboxID, tt.sandboxID)
			}
		})
	}
}

func TestCreateSandboxGeneratesIDWhenSandboxIDIsNotProvided(t *testing.T) {
	for _, sandboxID := range []string{"", " \t\n "} {
		t.Run(fmt.Sprintf("input_%q", sandboxID), func(t *testing.T) {
			ops := &fakeSandboxOps{}
			svc := New(ops, nil, nil)
			svc.SetSandboxDefaults(SandboxDefaults{VCPUNum: 2, VCPUMax: 2, RamMB: 1024})
			setFakeTemplate(svc, testTemplateName, digest.FromString("template-1").String(), conchtemplate.BootModeCold)

			result, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
				SandboxID:    sandboxID,
				TemplateName: testTemplateName,
			})
			if err != nil {
				t.Fatalf("CreateSandbox() error = %v", err)
			}
			if len(result.SandboxID) != 32 || id.Validate(result.SandboxID) != nil {
				t.Fatalf("generated sandbox ID = %q, want 32-character safe ID", result.SandboxID)
			}
			if ops.req.SandboxID != result.SandboxID {
				t.Fatalf("runtime sandbox ID = %q, want %q", ops.req.SandboxID, result.SandboxID)
			}
		})
	}
}

func TestCreateSandboxRejectsExistingGlobalIDBeforeRuntimeCreate(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: digest.FromString("existing-sandbox").String(),
	}); err != nil {
		t.Fatalf("UpsertSandbox() seed error = %v", err)
	}
	ops := &fakeSandboxOps{}
	svc := New(ops, nil, store)

	_, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID:    "sandbox-1",
		TemplateName: testTemplateName,
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
	})
	if !errors.Is(err, sandbox.ErrAlreadyExists) {
		t.Fatalf("CreateSandbox() error = %v, want sandbox.ErrAlreadyExists", err)
	}
	if ops.createCalls != 0 {
		t.Fatalf("runtime Create() calls = %d, want 0", ops.createCalls)
	}
}

func TestCreateSandboxAppliesConfiguredBackend(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil)
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

func TestCreateSandboxRejectsMissingTemplate(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil)
	_, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{TemplateName: " \n ", VCPUNum: 2, VCPUMax: 2, RamMB: 1024})
	if !errors.Is(err, sandbox.ErrInvalidArgument) {
		t.Fatalf("CreateSandbox() error = %v, want sandbox.ErrInvalidArgument", err)
	}
}

func TestCreateSandboxRejectsUnknownTemplateName(t *testing.T) {
	operations := &fakeSandboxOps{}
	service := New(operations, nil, nil)
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
	service := New(operations, nil, nil)

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
	service := New(operations, nil, nil)

	_, err := service.CreateSandbox(context.Background(), SandboxCreateOptions{TemplateID: "not-a-digest"})
	if !errors.Is(err, sandbox.ErrInvalidArgument) {
		t.Fatalf("CreateSandbox() error = %v, want ErrInvalidArgument", err)
	}
	if operations.createCalls != 0 {
		t.Fatalf("runtime Create() calls = %d, want 0", operations.createCalls)
	}
}

func TestCreateSandboxAcceptsTemplateIDWithoutNameRecord(t *testing.T) {
	ctx := context.Background()
	host := newRuntimeImageHost(t)
	templateID := buildColdBootIndex(t, host, "sandbox-create-by-id")
	operations := &fakeSandboxOps{createResult: sandbox.CreateResult{BootIndexDigest: templateID}}
	store := newTestStore(t)
	service := New(operations, host.Client(), store)
	service.SetSandboxDefaults(SandboxDefaults{TemplateName: "default-name"})

	result, err := service.CreateSandbox(ctx, SandboxCreateOptions{
		TemplateID: templateID,
		VCPUNum:    2,
		VCPUMax:    2,
		RamMB:      1024,
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	if result.TemplateName != "" || result.TemplateID != templateID {
		t.Fatalf("CreateSandbox() result = %#v", result)
	}
	if operations.req.TemplateID != templateID {
		t.Fatalf("runtime request = %#v", operations.req)
	}
	record, err := store.GetSandbox(ctx, result.SandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if record.SourceTemplateName != "" || record.SourceTemplateID != templateID {
		t.Fatalf("sandbox record = %#v", record)
	}
}

func TestCreateSandboxGeneratesSingleSandboxID(t *testing.T) {
	store := newTestStore(t)
	templateID := digest.FromString("template-1").String()
	sandboxOps := &fakeSandboxOps{createResult: sandbox.CreateResult{
		BootIndexDigest: templateID,
	}}
	svc := New(sandboxOps, nil, store)
	setFakeTemplate(svc, testTemplateName, templateID, conchtemplate.BootModeCold)
	result, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
		TemplateName: testTemplateName,
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
	})
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
	svc := New(sandboxOps, nil, nil)
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

func TestCreateSandboxEnforcesResourceLimits(t *testing.T) {
	tests := []struct {
		name string
		opts SandboxCreateOptions
	}{
		{name: "vcpu number", opts: SandboxCreateOptions{VCPUNum: runtimeapi.SandboxMaxVCPU + 1, VCPUMax: runtimeapi.SandboxMaxVCPU + 1, RamMB: 1024}},
		{name: "vcpu maximum", opts: SandboxCreateOptions{VCPUNum: 2, VCPUMax: runtimeapi.SandboxMaxVCPU + 1, RamMB: 1024}},
		{name: "memory", opts: SandboxCreateOptions{VCPUNum: 2, VCPUMax: 2, RamMB: runtimeapi.SandboxMaxRAMMB + 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeSandboxOps{}
			svc := New(ops, nil, nil)
			defaultDigest := digest.FromString("default-template").String()
			svc.SetSandboxDefaults(SandboxDefaults{TemplateName: testTemplateName})
			setFakeTemplate(svc, testTemplateName, defaultDigest, conchtemplate.BootModeCold)
			tt.opts.SandboxID = "sandbox-limited"
			_, err := svc.CreateSandbox(context.Background(), tt.opts)
			if !errors.Is(err, sandbox.ErrResourceExhausted) {
				t.Fatalf("CreateSandbox() error = %v, want sandbox.ErrResourceExhausted", err)
			}
			if ops.createCalls != 0 {
				t.Fatalf("runtime Create() calls = %d, want 0", ops.createCalls)
			}
		})
	}
}

func TestCreateSandboxPersistsNetworkPolicy(t *testing.T) {
	store := newTestStore(t)
	templateID := digest.FromString("default-template").String()
	sandboxOps := &fakeSandboxOps{createResult: sandbox.CreateResult{
		BootIndexDigest: templateID,
	}}
	svc := New(sandboxOps, nil, store)
	setFakeTemplate(svc, testTemplateName, templateID, conchtemplate.BootModeCold)
	policy := &netstack.SandboxNetworkConfig{DenyOut: []string{"192.0.2.10"}}

	if _, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
		SandboxID:    "sandbox-network",
		TemplateName: testTemplateName,
		VCPUNum:      2,
		VCPUMax:      2,
		RamMB:        1024,
		Network:      policy,
	}); err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	if sandboxOps.req.Network != policy {
		t.Fatalf("runtime network = %#v, want %#v", sandboxOps.req.Network, policy)
	}
	record, err := store.GetSandbox(context.Background(), "sandbox-network")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if !reflect.DeepEqual(record.Network, policy) {
		t.Fatalf("stored network = %#v, want %#v", record.Network, policy)
	}
}

func TestUpdateSandboxNetworkConfigReplacesPolicy(t *testing.T) {
	store := newTestStore(t)
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, store)
	if err := store.UpsertSandbox(context.Background(), state.SandboxRecord{
		SandboxID:                "sandbox-network",
		State:                    state.SandboxReady,
		CheckpointHeadTemplateID: digest.FromString("sandbox-network").String(),
		Network:                  &netstack.SandboxNetworkConfig{AllowOut: []string{"192.0.2.10"}},
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	replacement := &netstack.SandboxNetworkConfig{DenyOut: []string{"198.51.100.10"}}

	if err := svc.UpdateSandboxNetworkConfig(context.Background(), SandboxNetworkUpdateOptions{
		SandboxID: "sandbox-network",
		Network:   replacement,
	}); err != nil {
		t.Fatalf("UpdateSandboxNetworkConfig() error = %v", err)
	}
	if sandboxOps.updateReq.Network != replacement {
		t.Fatalf("runtime network = %#v, want %#v", sandboxOps.updateReq.Network, replacement)
	}
	record, err := store.GetSandbox(context.Background(), "sandbox-network")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if !reflect.DeepEqual(record.Network, replacement) {
		t.Fatalf("stored network = %#v, want %#v", record.Network, replacement)
	}
}

func TestUpdateSandboxNetworkConfigAllowsSuspendedSandbox(t *testing.T) {
	store := newTestStore(t)
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, store)
	if err := store.UpsertSandbox(context.Background(), state.SandboxRecord{
		SandboxID:                "sandbox-suspended",
		State:                    state.SandboxSuspended,
		CheckpointHeadTemplateID: digest.FromString("sandbox-suspended").String(),
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	policy := &netstack.SandboxNetworkConfig{DenyIn: []string{"192.0.2.10"}}

	if err := svc.UpdateSandboxNetworkConfig(context.Background(), SandboxNetworkUpdateOptions{
		SandboxID: "sandbox-suspended",
		Network:   policy,
	}); err != nil {
		t.Fatalf("UpdateSandboxNetworkConfig() error = %v", err)
	}
	if sandboxOps.updateReq.Network != policy {
		t.Fatalf("runtime network = %#v, want %#v", sandboxOps.updateReq.Network, policy)
	}
}

func TestCreateTemplateRequiresContainerdClient(t *testing.T) {
	store := newTestStore(t)
	svc := New(nil, nil, store)

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

	svc := New(nil, host.Client(), newTestStore(t))
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
	store := newTestStore(t)
	svc := New(nil, host.Client(), store)
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
