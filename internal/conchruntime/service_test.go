package conchruntime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/sandbox"
	runtimeSnapshot "github.com/openeuler/Conch/internal/snapshot"
)

type fakeSandboxOps struct {
	req           sandbox.SandboxCreateRequest
	createCalls   int
	createStarted chan struct{}
	createRelease chan struct{}
	createErr     error
	deleteErr     error
	deleteCalls   int
	deleteReq     sandbox.SandboxDeleteRequest
	deleteStarted chan struct{}
	deleteRelease chan struct{}
	pauseID       string
	pauseErr      error
	pauseReq      sandbox.SandboxPauseRequest
	pauseStarted  chan struct{}
	pauseRelease  chan struct{}
}

type failReadyTransitionStore struct {
	*state.BoltStore
}

func (s *failReadyTransitionStore) TransitionSandbox(
	ctx context.Context,
	id, expected, next string,
	replacement *state.SandboxRecord,
) (state.SandboxRecord, error) {
	if expected == state.SandboxCreating && next == state.SandboxReady {
		_ = s.BoltStore.DeleteSandbox(ctx, id)
		return state.SandboxRecord{}, fmt.Errorf("%w: injected ready transition failure", state.ErrNotFound)
	}
	return s.BoltStore.TransitionSandbox(ctx, id, expected, next, replacement)
}

func (f *fakeSandboxOps) Create(req sandbox.SandboxCreateRequest) (sandbox.SandboxCreateResult, error) {
	f.createCalls++
	f.req = req
	if f.createStarted != nil {
		close(f.createStarted)
		<-f.createRelease
	}
	if f.createErr != nil {
		return sandbox.SandboxCreateResult{}, f.createErr
	}
	return sandbox.SandboxCreateResult{
		Namespace:  req.Namespace,
		SandboxID:  req.SandboxId,
		IP:         "192.0.2.10",
		AgentToken: req.AgentToken,
	}, nil
}

func TestCreateSandboxDeletesReservationAfterConfirmedRollback(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	svc := New(&fakeSandboxOps{createErr: &sandbox.CreateFailure{
		Err:             errors.New("create failed"),
		CleanupComplete: true,
	}}, nil, store, "default")

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{}); err == nil {
		t.Fatal("CreateSandbox() error = nil")
	}
	records, err := store.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("ListSandboxes() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("sandboxes = %#v, want none", records)
	}
}

func TestCreateSandboxRetainsReservationAfterIncompleteRollback(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	svc := New(&fakeSandboxOps{createErr: &sandbox.CreateFailure{
		Err:             errors.New("create failed"),
		CleanupComplete: false,
	}}, nil, store, "default")

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{}); err == nil {
		t.Fatal("CreateSandbox() error = nil")
	}
	records, err := store.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("ListSandboxes() error = %v", err)
	}
	if len(records) != 1 || records[0].State != state.SandboxUnknown || records[0].LastError == "" {
		t.Fatalf("sandboxes = %#v, want failed UNKNOWN record", records)
	}
}

func TestCreateSandboxRollbackDeletesSandboxWhenReadyTransitionLosesRecord(t *testing.T) {
	ctx := context.Background()
	baseStore := newTestStore(t)
	store := &failReadyTransitionStore{BoltStore: baseStore}
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, store, "default")

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{}); err == nil {
		t.Fatal("CreateSandbox() error = nil")
	}
	if sandboxOps.deleteCalls != 1 {
		t.Fatalf("Delete() calls = %d, want 1", sandboxOps.deleteCalls)
	}
	records, err := baseStore.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("ListSandboxes() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("sandboxes = %#v, want none", records)
	}
}

func (f *fakeSandboxOps) Delete(req sandbox.SandboxDeleteRequest) error {
	f.deleteCalls++
	f.deleteReq = req
	if f.deleteStarted != nil {
		close(f.deleteStarted)
		<-f.deleteRelease
	}
	return f.deleteErr
}

func (f *fakeSandboxOps) Pause(req sandbox.SandboxPauseRequest) (string, error) {
	f.pauseReq = req
	if f.pauseStarted != nil {
		close(f.pauseStarted)
		<-f.pauseRelease
	}
	return f.pauseID, f.pauseErr
}

func TestCreateSandboxReservesBeforeStartingRuntime(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	sandboxOps := &fakeSandboxOps{
		createStarted: make(chan struct{}),
		createRelease: make(chan struct{}),
	}
	svc := New(sandboxOps, nil, store, "default")

	firstDone := make(chan error, 1)
	go func() {
		_, err := svc.CreateSandbox(ctx, SandboxCreateOptions{})
		firstDone <- err
	}()
	<-sandboxOps.createStarted

	records, err := store.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("ListSandboxes() while creating error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len(ListSandboxes()) = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.State != state.SandboxCreating {
		t.Fatalf("sandbox.State while creating = %q, want %q", rec.State, state.SandboxCreating)
	}
	close(sandboxOps.createRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("CreateSandbox() first error = %v", err)
	}
	if sandboxOps.createCalls != 1 {
		t.Fatalf("Create() calls = %d, want 1", sandboxOps.createCalls)
	}

	rec, err = store.GetSandbox(ctx, rec.PodSandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec.State != state.SandboxReady {
		t.Fatalf("sandbox.State = %q, want %q", rec.State, state.SandboxReady)
	}
}

func TestPauseSandboxPromotesSnapshotReturnedByManager(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "sandbox-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "default",
		State:          state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	svc := New(&fakeSandboxOps{pauseID: "snapshot-1"}, nil, store, "default")
	snapshotID, err := svc.PauseSandbox(ctx, "default", "sandbox-1")
	if err != nil {
		t.Fatalf("PauseSandbox() error = %v", err)
	}
	if snapshotID != "snapshot-1" {
		t.Fatalf("PauseSandbox() snapshot ID = %q, want snapshot-1", snapshotID)
	}
	rec, err := store.GetSandbox(ctx, "sandbox-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if rec.State != state.SandboxStopped || rec.SnapshotID != "snapshot-1" {
		t.Fatalf("sandbox record = %#v, want stopped snapshot", rec)
	}
}

func TestPauseSandboxUsesRecordedNamespaceWhenRequestNamespaceEmpty(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "team-a",
		State:          state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	sandboxOps := &fakeSandboxOps{pauseID: "snapshot-1"}
	svc := New(sandboxOps, nil, store, "default")
	if _, err := svc.PauseSandbox(ctx, "", "pod-1"); err != nil {
		t.Fatalf("PauseSandbox() error = %v", err)
	}
	if sandboxOps.pauseReq.Namespace != "team-a" {
		t.Fatalf("Pause() namespace = %q, want team-a", sandboxOps.pauseReq.Namespace)
	}
	if sandboxOps.pauseReq.SandboxId != "sandbox-1" {
		t.Fatalf("Pause() sandbox id = %q, want sandbox-1", sandboxOps.pauseReq.SandboxId)
	}
}

func TestPauseSandboxRejectsDuplicateOperation(t *testing.T) {
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

	sandboxOps := &fakeSandboxOps{
		pauseID:      "snapshot-1",
		pauseStarted: make(chan struct{}),
		pauseRelease: make(chan struct{}),
	}
	svc := New(sandboxOps, nil, store, "default")

	firstDone := make(chan error, 1)
	go func() {
		_, err := svc.PauseSandbox(ctx, "default", "pod-1")
		firstDone <- err
	}()
	<-sandboxOps.pauseStarted

	if _, err := svc.PauseSandbox(ctx, "default", "pod-1"); !errors.Is(err, state.ErrStateConflict) {
		t.Fatalf("PauseSandbox() duplicate error = %v, want ErrStateConflict", err)
	}

	close(sandboxOps.pauseRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("PauseSandbox() first error = %v", err)
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

func TestRemoveSandboxUsesRecordedNamespaceWhenRequestNamespaceEmpty(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "team-a",
		State:          state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}

	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, store, "default")
	if err := svc.RemoveSandbox(ctx, "", "pod-1"); err != nil {
		t.Fatalf("RemoveSandbox() error = %v", err)
	}
	if sandboxOps.deleteReq.Namespace != "team-a" {
		t.Fatalf("Delete() namespace = %q, want team-a", sandboxOps.deleteReq.Namespace)
	}
	if sandboxOps.deleteReq.SandboxId != "sandbox-1" {
		t.Fatalf("Delete() sandbox id = %q, want sandbox-1", sandboxOps.deleteReq.SandboxId)
	}
}

func TestRemoveSandboxRejectsDuplicateOperation(t *testing.T) {
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

	sandboxOps := &fakeSandboxOps{
		deleteStarted: make(chan struct{}),
		deleteRelease: make(chan struct{}),
	}
	svc := New(sandboxOps, nil, store, "default")

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- svc.RemoveSandbox(ctx, "default", "pod-1")
	}()
	<-sandboxOps.deleteStarted

	if err := svc.RemoveSandbox(ctx, "default", "pod-1"); !errors.Is(err, state.ErrStateConflict) {
		t.Fatalf("RemoveSandbox() duplicate error = %v, want ErrStateConflict", err)
	}

	close(sandboxOps.deleteRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("RemoveSandbox() first error = %v", err)
	}
}

func TestHydrateSandboxStatesLoadsTransientStates(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	records := []state.SandboxRecord{
		{PodSandboxID: "creating-1", ConchSandboxID: "creating-1", Namespace: "team-a", State: state.SandboxCreating},
		{PodSandboxID: "pausing-1", ConchSandboxID: "pausing-1", Namespace: "", State: state.SandboxPausing},
		{PodSandboxID: "deleting-1", ConchSandboxID: "deleting-1", Namespace: "team-b", State: state.SandboxDeleting},
		{PodSandboxID: "ready-1", ConchSandboxID: "ready-1", Namespace: "team-c", State: state.SandboxReady},
	}
	for _, rec := range records {
		if err := store.UpsertSandbox(ctx, rec); err != nil {
			t.Fatalf("UpsertSandbox(%q) error = %v", rec.PodSandboxID, err)
		}
	}

	svc := New(nil, nil, store, "default")
	if err := svc.HydrateSandboxStates(ctx); err != nil {
		t.Fatalf("HydrateSandboxStates() error = %v", err)
	}

	for _, item := range []struct {
		namespace string
		id        string
	}{
		{namespace: "team-a", id: "creating-1"},
		{namespace: "default", id: "pausing-1"},
		{namespace: "team-b", id: "deleting-1"},
	} {
		if _, err := svc.setSandboxState(item.namespace, item.id, state.SandboxReady, state.SandboxDeleting, state.SandboxReady); !errors.Is(err, state.ErrStateConflict) {
			t.Fatalf("setSandboxState(%q, %q) error = %v, want ErrStateConflict", item.namespace, item.id, err)
		}
	}

	mapKey, err := svc.setSandboxState("team-c", "ready-1", state.SandboxReady, state.SandboxDeleting, state.SandboxReady)
	if err != nil {
		t.Fatalf("setSandboxState() ready sandbox error = %v", err)
	}
	svc.sandboxStates.Delete(mapKey)
}

func TestStopSandboxRejectsAbsentState(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("sandbox not found")}
	svc := New(sandboxOps, nil, store, "default")
	if err := svc.StopSandbox(ctx, "default", "missing-pod"); !errors.Is(err, state.ErrStateConflict) {
		t.Fatalf("StopSandbox() error = %v, want ErrStateConflict", err)
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

func TestDeleteSandboxRuntimeStateRemovesLastViewSnapshotRef(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	svc := New(nil, nil, store, "default")
	namespace := "default"
	sandboxID := "sandbox-1"
	parentID := "parent-rootfs"

	if err := store.UpsertSnapshotRuntime(ctx, state.SnapshotRuntimeRecord{
		Namespace: namespace,
		SandboxID: sandboxID,
		State:     state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSnapshotRuntime() error = %v", err)
	}
	if err := store.UpsertViewSnapshot(ctx, state.ViewSnapshotRecord{
		Namespace:        namespace,
		ParentSnapshotID: parentID,
		ViewSnapshotKey:  "view-rootfs",
		RefCount:         1,
		State:            state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertViewSnapshot() error = %v", err)
	}
	if err := store.UpsertViewAlias(ctx, state.ViewAliasRecord{
		Namespace:        namespace,
		AliasKey:         runtimeSnapshot.RootfsViewAliasKey(sandboxID),
		SandboxID:        sandboxID,
		ParentSnapshotID: parentID,
	}); err != nil {
		t.Fatalf("UpsertViewAlias() error = %v", err)
	}

	if err := svc.deleteSandboxRuntimeState(ctx, namespace, sandboxID); err != nil {
		t.Fatalf("deleteSandboxRuntimeState() error = %v", err)
	}

	views, err := store.ListViewSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListViewSnapshots() error = %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("view snapshots = %#v, want none", views)
	}
	aliases, err := store.ListViewAliases(ctx)
	if err != nil {
		t.Fatalf("ListViewAliases() error = %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("view aliases = %#v, want none", aliases)
	}
	runtimes, err := store.ListSnapshotRuntimes(ctx)
	if err != nil {
		t.Fatalf("ListSnapshotRuntimes() error = %v", err)
	}
	if len(runtimes) != 0 {
		t.Fatalf("snapshot runtimes = %#v, want none", runtimes)
	}
}

func TestDeleteSandboxRuntimeStateDecrementsSharedViewSnapshotRef(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	svc := New(nil, nil, store, "default")
	namespace := "default"
	parentID := "parent-rootfs"

	if err := store.UpsertViewSnapshot(ctx, state.ViewSnapshotRecord{
		Namespace:        namespace,
		ParentSnapshotID: parentID,
		ViewSnapshotKey:  "view-rootfs",
		RefCount:         2,
		State:            state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertViewSnapshot() error = %v", err)
	}
	for _, sandboxID := range []string{"sandbox-1", "sandbox-2"} {
		if err := store.UpsertViewAlias(ctx, state.ViewAliasRecord{
			Namespace:        namespace,
			AliasKey:         runtimeSnapshot.RootfsViewAliasKey(sandboxID),
			SandboxID:        sandboxID,
			ParentSnapshotID: parentID,
		}); err != nil {
			t.Fatalf("UpsertViewAlias(%s) error = %v", sandboxID, err)
		}
	}

	if err := svc.deleteSandboxRuntimeState(ctx, namespace, "sandbox-1"); err != nil {
		t.Fatalf("deleteSandboxRuntimeState() error = %v", err)
	}

	views, err := store.ListViewSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListViewSnapshots() error = %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("view snapshots = %#v, want one", views)
	}
	if views[0].ParentSnapshotID != parentID || views[0].RefCount != 1 {
		t.Fatalf("view snapshot = %#v, want parent %q refCount 1", views[0], parentID)
	}
	aliases, err := store.ListViewAliases(ctx)
	if err != nil {
		t.Fatalf("ListViewAliases() error = %v", err)
	}
	if len(aliases) != 1 || aliases[0].AliasKey != runtimeSnapshot.RootfsViewAliasKey("sandbox-2") {
		t.Fatalf("view aliases = %#v, want sandbox-2 alias only", aliases)
	}
}

func TestUpsertSandboxRuntimeStateRecordsResumeMemAlias(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	svc := New(nil, nil, store, "default")

	rec := state.SandboxRecord{
		Namespace:      "default",
		ConchSandboxID: "sandbox-1",
		LeaseID:        "lease-1",
		ParentRootfsID: "parent-rootfs",
		ParentMemID:    "parent-mem",
		ParentVMID:     "parent-vm",
		RootfsMount:    "/tmp/rootfs",
		MemMount:       "/tmp/mem",
		VMMount:        "/tmp/vm",
	}
	createResult := sandbox.SandboxCreateResult{
		Resume: true,
	}

	if err := svc.upsertSandboxRuntimeState(ctx, rec, createResult); err != nil {
		t.Fatalf("upsertSandboxRuntimeState() error = %v", err)
	}

	aliases, err := store.ListViewAliases(ctx)
	if err != nil {
		t.Fatalf("ListViewAliases() error = %v", err)
	}
	want := map[string]struct{}{
		runtimeSnapshot.RootfsViewAliasKey("sandbox-1"): {},
		runtimeSnapshot.MemViewAliasKey("sandbox-1"):    {},
		runtimeSnapshot.VMViewAliasKey("sandbox-1"):     {},
	}
	if len(aliases) != len(want) {
		t.Fatalf("view aliases = %#v, want %#v", aliases, want)
	}
	for _, alias := range aliases {
		if _, ok := want[alias.AliasKey]; !ok {
			t.Fatalf("unexpected view alias = %#v", alias)
		}
	}
}

func TestCreateSandboxAppliesDefaults(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil, "default")
	svc.SetSandboxDefaults(SandboxDefaults{
		ImageName: "registry.example.invalid/conch/sandbox:latest",
		VMMName:   "cloud-hypervisor",
		VCPUNum:   2,
		VCPUMax:   4,
		RamMB:     4096,
	})

	result, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{})
	if err != nil {
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
	if sandboxOps.req.AgentToken == "" {
		t.Fatal("AgentToken is empty")
	}
	if sandboxOps.req.SandboxId == "" || result.SandboxID != sandboxOps.req.SandboxId {
		t.Fatalf("generated sandbox IDs: request=%q result=%q", sandboxOps.req.SandboxId, result.SandboxID)
	}
	if result.AgentToken != sandboxOps.req.AgentToken {
		t.Fatalf("result.AgentToken = %q, want generated token", result.AgentToken)
	}
}

func TestCreateSandboxKeepsExplicitOptions(t *testing.T) {
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil, "default")
	svc.SetSandboxDefaults(SandboxDefaults{
		ImageName: "default-image",
		VMMName:   "default-vmm",
		VCPUNum:   2,
		VCPUMax:   2,
		RamMB:     4096,
	})

	_, err := svc.CreateSandbox(context.Background(), SandboxCreateOptions{
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
