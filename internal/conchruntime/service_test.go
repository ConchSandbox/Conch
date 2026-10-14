package conchruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandboxproxy"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

// fakeSandboxOps stands in for sandbox.Manager. When store is set it mimics
// Manager's record ownership: Create persists CREATING then READY, a failed
// Create with retained cleanup keeps the record, and Delete removes it.
type fakeSandboxOps struct {
	req          sandbox.CreateRequest
	createResult runtimeapi.SandboxCreateResult
	createErr    error
	createCalls  int
	deleteErr    error
	deleteCalls  int
	releaseErr   error
	releaseCalls int
	// releaseHook runs inside a successful ReleaseRuntime, simulating a guest
	// exit whose cleanup persists UNKNOWN while the release is in flight.
	releaseHook func(context.Context, string)
	resumeCalls int
	createHook  func(context.Context)
	// afterCreate runs after the READY record is persisted, simulating a
	// runtime exit that lands between Manager.Create and the caller's next
	// read of the record.
	afterCreate func(context.Context, string)
	store       sandbox.Store
	// retainOnCreateFailure keeps the CREATING record when Create fails,
	// mimicking a Manager cleanup that could not confirm resource release.
	retainOnCreateFailure bool
	checkpoint            sandbox.CheckpointResult
	checkpointErr         error
	updateReq             sandbox.NetworkUpdateRequest
	updateErr             error
}

func (f *fakeSandboxOps) Create(ctx context.Context, req sandbox.CreateRequest) (runtimeapi.SandboxCreateResult, error) {
	f.req = req
	f.createCalls++
	if f.createHook != nil {
		f.createHook(ctx)
	}
	result := f.createResult
	if result.SandboxID == "" {
		result.SandboxID = req.SandboxID
	}
	if result.AgentToken == "" {
		result.AgentToken = req.AgentToken
	}
	head := result.TemplateID
	if head == "" {
		head = req.TemplateID
	}
	if f.store != nil {
		rec := sandbox.Record{
			ID: req.SandboxID, RuntimeID: req.RuntimeID, State: sandbox.StateReady,
			CreatedAt:          time.Now().UnixNano(),
			SourceTemplateName: req.TemplateName, SourceTemplateID: req.TemplateID,
			CheckpointHeadTemplateID: head, IP: result.IP,
			VCPUNum: req.VCPUNum, RamMB: req.RAMMB,
		}
		if f.createErr != nil {
			if !f.retainOnCreateFailure {
				return result, f.createErr
			}
			rec.State = sandbox.StateUnknown
			rec.LastError = f.createErr.Error()
			if _, err := f.store.Create(ctx, rec); err != nil {
				return runtimeapi.SandboxCreateResult{}, err
			}
			return result, f.createErr
		}
		if _, err := f.store.Create(ctx, rec); err != nil {
			return runtimeapi.SandboxCreateResult{}, err
		}
	}
	if f.afterCreate != nil {
		f.afterCreate(ctx, req.SandboxID)
	}
	return result, f.createErr
}

func (f *fakeSandboxOps) Delete(ctx context.Context, sandboxID string) error {
	f.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if f.store != nil {
		return f.store.Delete(ctx, sandboxID)
	}
	return nil
}

// ReleaseRuntime mimics Manager.ReleaseRuntime: the record survives the
// release, and a failed release retains it as UNKNOWN with the error.
func (f *fakeSandboxOps) ReleaseRuntime(ctx context.Context, sandboxID string) error {
	f.releaseCalls++
	if f.releaseErr == nil {
		if f.releaseHook != nil {
			f.releaseHook(ctx, sandboxID)
		}
		return nil
	}
	if f.store != nil {
		if rec, err := f.store.Get(ctx, sandboxID); err == nil {
			rec.State = sandbox.StateUnknown
			rec.LastError = f.releaseErr.Error()
			if _, err := f.store.Update(ctx, rec); err != nil {
				return err
			}
		}
	}
	return f.releaseErr
}

func (f *fakeSandboxOps) Suspend(context.Context, string) error { return nil }

// Resume mimics Manager.Resume: only a SUSPENDED record returns to READY.
func (f *fakeSandboxOps) Resume(ctx context.Context, sandboxID string) error {
	f.resumeCalls++
	if f.store == nil {
		return nil
	}
	rec, err := f.store.Get(ctx, sandboxID)
	if err != nil {
		return err
	}
	if rec.State != sandbox.StateSuspended {
		return sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
	rec.State = sandbox.StateReady
	rec.LastError = ""
	_, err = f.store.Update(ctx, rec)
	return err
}

func (f *fakeSandboxOps) UpdateNetwork(_ context.Context, req sandbox.NetworkUpdateRequest) error {
	f.updateReq = req
	return f.updateErr
}
func (f *fakeSandboxOps) Checkpoint(ctx context.Context, sandboxID string, register func(context.Context, sandbox.CheckpointResult) error) (sandbox.CheckpointResult, error) {
	if f.checkpointErr != nil {
		// ErrCheckpointResume leaves the sandbox SUSPENDED, matching
		// captureCheckpoint's persistence on that error.
		if errors.Is(f.checkpointErr, sandbox.ErrCheckpointResume) && f.store != nil {
			if rec, err := f.store.Get(ctx, sandboxID); err == nil {
				rec.State, rec.LastError = sandbox.StateSuspended, f.checkpointErr.Error()
				_, _ = f.store.Update(ctx, rec)
			}
		}
		return sandbox.CheckpointResult{}, f.checkpointErr
	}
	return f.checkpoint, register(ctx, f.checkpoint)
}

func TestNativeCreateDoesNotReserveE2BCapacity(t *testing.T) {
	ctx := context.Background()
	store := newMemorySandboxStore()
	templateID := digest.FromString("native-capacity-template").String()
	ops := &fakeSandboxOps{store: store}
	svc := New(ops, nil)
	svc.Store = store
	setFakeTemplate(svc, testTemplateName, templateID, conchtemplate.BootModeCold)
	var err error
	svc.Capacity, err = NewCapacity(CapacityLimits{MaxSandboxes: 1, MaxCPUs: 2, MaxMemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID: "native-capacity", TemplateName: testTemplateName, VCPUNum: 2, VCPUMax: 2, RamMB: 512,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Capacity.reserve("e2b-capacity", 2, 512); err != nil {
		t.Fatalf("native create consumed E2B capacity: %v", err)
	}
}

func TestPauseKeepsProxyRouteWhenRuntimeTeardownFails(t *testing.T) {
	ctx := context.Background()
	store := newMemorySandboxStore()
	ops := &fakeSandboxOps{
		store: store, releaseErr: errors.New("runtime teardown failed"),
		checkpoint: sandbox.CheckpointResult{BootIndexDigest: digest.FromString("pause-route-head").String()},
	}
	svc := New(ops, nil)
	svc.Store = store
	svc.ProxyRoutes = sandboxproxy.NewRegistry()
	record, err := store.Create(ctx, sandbox.Record{
		ID: "pause-route", RuntimeID: "runtime-pause", State: sandbox.StateReady, E2B: true,
		CheckpointHeadTemplateID: digest.FromString("pause-route-template").String(), VCPUNum: 1, RamMB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	generation := svc.ProxyRoutes.Begin(record.ID)
	if err := svc.ProxyRoutes.Publish(record.ID, generation, "192.0.2.11"); err != nil {
		t.Fatal(err)
	}
	if err := svc.PauseSandbox(ctx, record.ID, time.Time{}); err == nil {
		t.Fatal("PauseSandbox succeeded after teardown failure")
	}
	if _, ok := svc.ProxyRoutes.Lookup(record.ID); !ok {
		t.Fatal("pause teardown failure removed the live proxy route")
	}
}

// TestUpdateNetworkKeepsCreateTimeHostMask proves an egress-policy update
// cannot clear the maskRequestHost captured at create time: the E2B update
// contract carries no mask field, so the record and the republished route
// keep the original mask.
func TestUpdateNetworkKeepsCreateTimeHostMask(t *testing.T) {
	ctx := context.Background()
	store := newMemorySandboxStore()
	ops := &fakeSandboxOps{store: store}
	svc := New(ops, nil)
	svc.Store = store
	svc.ProxyRoutes = sandboxproxy.NewRegistry()
	record, err := store.Create(ctx, sandbox.Record{
		ID: "update-mask", RuntimeID: "runtime-update", State: sandbox.StateReady, E2B: true,
		CheckpointHeadTemplateID: digest.FromString("update-mask-template").String(),
		VCPUNum:                  1, RamMB: 128, MaskRequestHost: "api.example.com:443",
	})
	if err != nil {
		t.Fatal(err)
	}
	generation := svc.ProxyRoutes.Begin(record.ID)
	if err := svc.ProxyRoutes.PublishWithHostMask(record.ID, generation, "192.0.2.12", "api.example.com:443"); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateSandboxNetworkConfig(ctx, SandboxNetworkUpdateOptions{
		SandboxID: record.ID,
		Network:   &runtimeapi.SandboxNetworkConfig{DenyOut: []string{"0.0.0.0/0"}},
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.MaskRequestHost != "api.example.com:443" {
		t.Fatalf("record mask = %q, want the create-time mask", updated.MaskRequestHost)
	}
	route, ok := svc.ProxyRoutes.Lookup(record.ID)
	if !ok {
		t.Fatal("update removed the proxy route")
	}
	if route.MaskRequestHost != "api.example.com:443" {
		t.Fatalf("route mask = %q, want the create-time mask", route.MaskRequestHost)
	}
}

// TestExpiryPauseRevalidatesDeadline proves the expiredBefore guard: a TTL
// extension that landed after the expiry tick must stop the auto-pause, while
// a deadline still due at that instant pauses normally.
func TestExpiryPauseRevalidatesDeadline(t *testing.T) {
	ctx := context.Background()
	store := newMemorySandboxStore()
	ops := &fakeSandboxOps{
		store:      store,
		checkpoint: sandbox.CheckpointResult{BootIndexDigest: digest.FromString("expiry-head").String()},
	}
	svc := New(ops, nil)
	svc.Store = store
	svc.ProxyRoutes = sandboxproxy.NewRegistry()
	tick := time.Now()
	record, err := store.Create(ctx, sandbox.Record{
		ID: "expiry-revalidate", RuntimeID: "runtime-expiry", State: sandbox.StateReady, E2B: true,
		CheckpointHeadTemplateID: digest.FromString("expiry-template").String(), VCPUNum: 1, RamMB: 128,
		ExpiresAt: tick.Add(-time.Second).UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The client refreshed the deadline after the tick observed the expiry.
	refreshed, err := store.Update(ctx, sandbox.Record{
		ID: record.ID, RuntimeID: record.RuntimeID, State: sandbox.StateReady, E2B: true,
		CheckpointHeadTemplateID: record.CheckpointHeadTemplateID, VCPUNum: 1, RamMB: 128,
		ExpiresAt: tick.Add(time.Minute).UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = svc.PauseSandbox(ctx, record.ID, tick)
	if !errors.Is(err, sandbox.ErrFailedPrecondition) {
		t.Fatalf("refreshed pause error = %v, want FailedPrecondition", err)
	}
	after, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != sandbox.StateReady || after.ExpiresAt != refreshed.ExpiresAt {
		t.Fatalf("rejected pause mutated the record: %#v", after)
	}

	// A deadline still due at the tick instant pauses normally. Use the exact
	// deadline as the tick: due-at-that-instant must pass the guard.
	if err := svc.PauseSandbox(ctx, record.ID, time.Unix(0, refreshed.ExpiresAt)); err != nil {
		t.Fatalf("expired pause = %v", err)
	}
	paused, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != sandbox.StatePaused || paused.ExpiresAt != 0 {
		t.Fatalf("paused record = %#v", paused)
	}
}

// TestPauseResumesSandboxWhenCheckpointResumeFails proves the ErrCheckpointResume
// fallback: the sandbox is un-suspended so it keeps running under its READY
// record instead of being stranded in SUSPENDED.
func TestPauseResumesSandboxWhenCheckpointResumeFails(t *testing.T) {
	ctx := context.Background()
	store := newMemorySandboxStore()
	ops := &fakeSandboxOps{
		store:         store,
		checkpointErr: fmt.Errorf("capture: %w", sandbox.ErrCheckpointResume),
	}
	svc := New(ops, nil)
	svc.Store = store
	svc.ProxyRoutes = sandboxproxy.NewRegistry()
	record, err := store.Create(ctx, sandbox.Record{
		ID: "pause-resume-fail", RuntimeID: "runtime-resume-fail", State: sandbox.StateReady, E2B: true,
		CheckpointHeadTemplateID: digest.FromString("resume-fail-template").String(), VCPUNum: 1, RamMB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = svc.PauseSandbox(ctx, record.ID, time.Time{})
	if !errors.Is(err, sandbox.ErrCheckpointResume) {
		t.Fatalf("pause error = %v, want ErrCheckpointResume", err)
	}
	if ops.resumeCalls != 1 {
		t.Fatalf("resume calls = %d, want 1", ops.resumeCalls)
	}
	after, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != sandbox.StateReady {
		t.Fatalf("record state after failed checkpoint = %s, want READY", after.State)
	}
	if ops.releaseCalls != 0 {
		t.Fatalf("release calls after failed checkpoint = %d, want 0", ops.releaseCalls)
	}
}

// TestPauseHealsRecordWhenGuestExitsDuringRelease proves the post-release
// heal: a guest exit that persisted UNKNOWN between the PAUSED commit and the
// runtime release must not strand the sandbox — the record is rewritten to
// the pause outcome the caller was told about.
func TestPauseHealsRecordWhenGuestExitsDuringRelease(t *testing.T) {
	ctx := context.Background()
	store := newMemorySandboxStore()
	ops := &fakeSandboxOps{
		store:      store,
		checkpoint: sandbox.CheckpointResult{BootIndexDigest: digest.FromString("pause-heal-head").String()},
		// Mirror cleanupExitedSandbox: it reads the current record — already
		// PAUSED with the RuntimeID cleared — persists UNKNOWN and clears
		// RuntimeSnapshots, without checking whether the record was paused.
		releaseHook: func(ctx context.Context, sandboxID string) {
			rec, err := store.Get(ctx, sandboxID)
			if err != nil {
				t.Fatal(err)
			}
			if rec.State != sandbox.StatePaused {
				t.Fatalf("release observed state %s, want PAUSED committed first", rec.State)
			}
			rec.State = sandbox.StateUnknown
			rec.LastError = ""
			rec.RuntimeSnapshots = nil
			if _, err := store.Update(ctx, rec); err != nil {
				t.Fatal(err)
			}
		},
	}
	svc := New(ops, nil)
	svc.Store = store
	svc.ProxyRoutes = sandboxproxy.NewRegistry()
	record, err := store.Create(ctx, sandbox.Record{
		ID: "pause-heal", RuntimeID: "runtime-heal", State: sandbox.StateReady, E2B: true,
		CheckpointHeadTemplateID: digest.FromString("pause-heal-template").String(), VCPUNum: 1, RamMB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.PauseSandbox(ctx, record.ID, time.Time{}); err != nil {
		t.Fatalf("pause with racing exit = %v", err)
	}
	after, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != sandbox.StatePaused {
		t.Fatalf("record after racing exit = %s, want PAUSED", after.State)
	}
	if after.RuntimeID != "" || after.ExpiresAt != 0 || after.LastError != "" {
		t.Fatalf("healed record kept runtime fields: %#v", after)
	}
	if after.CheckpointHeadTemplateID != ops.checkpoint.BootIndexDigest {
		t.Fatalf("healed record head = %q, want %q", after.CheckpointHeadTemplateID, ops.checkpoint.BootIndexDigest)
	}
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
		explicitName: {Name: explicitName, Origin: conchtemplate.OriginImage, BootMode: conchtemplate.BootModeCold, BootIndexDigest: explicitDigest},
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
			ops := &fakeSandboxOps{createResult: runtimeapi.SandboxCreateResult{
				IP: "192.0.2.10", AgentToken: "runtime-token", TemplateID: want,
			}}
			svc := New(ops, nil)
			setFakeTemplate(svc, testTemplateName, want, conchtemplate.BootModeCold)
			opts := SandboxCreateOptions{TemplateID: want, VCPUNum: 2, VCPUMax: 2, RamMB: 512}
			if byName {
				opts.TemplateID = ""
				opts.TemplateName = testTemplateName
			}
			got, err := svc.CreateSandbox(context.Background(), opts)
			if err != nil || got.SandboxID != ops.req.SandboxID || got.AgentToken != "runtime-token" ||
				got.IP != "192.0.2.10" || ops.req.TemplateID != want || ops.createCalls != 1 {
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

func TestColdTemplateKeepsRequestResources(t *testing.T) {
	ctx := containerdclient.NewNamespaceContext(context.Background())
	host := newRuntimeImageHost(t)
	digest := buildColdBootIndex(t, host, "capacity-keep-cold")
	const name = "capacity-keep"
	seedTemplate(t, ctx, host, name, digest, conchtemplate.BootModeCold)
	ops := &fakeSandboxOps{}
	svc := New(ops, host.Client())
	svc.Templates = host.TemplateStore()
	svc.SetSandboxDefaults(SandboxDefaults{VMMName: "cloud-hypervisor", VCPUNum: 2, VCPUMax: 2, RamMB: 128})

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{TemplateName: name, VCPUNum: 4, VCPUMax: 4, RamMB: 4096}); err != nil {
		t.Fatal(err)
	}
	if ops.req.VCPUNum != 4 || ops.req.RAMMB != 4096 {
		t.Fatalf("runtime allocation=%+v", ops.req)
	}
}
