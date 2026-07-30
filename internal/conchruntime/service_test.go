package conchruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/runtimeapi"
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
	updateNetworkReq   sandbox.NetworkUpdateRequest
	updateNetworkErr   error
}

type serializedDeleteOps struct {
	fakeSandboxOps
	firstEntered chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int32
}

type serializedNetworkUpdateOps struct {
	fakeSandboxOps
	updateEntered chan struct{}
	releaseUpdate chan struct{}
	deleteEntered chan struct{}
}

type rollbackNetworkUpdateOps struct {
	fakeSandboxOps
	requests []sandbox.NetworkUpdateRequest
	errs     []error
}

func (f *serializedDeleteOps) Delete(sandbox.DeleteRequest) error {
	if f.calls.Add(1) == 1 {
		close(f.firstEntered)
		<-f.releaseFirst
		return nil
	}
	return errors.New("sandbox not found")
}

func (f *serializedNetworkUpdateOps) UpdateNetwork(_ context.Context, req sandbox.NetworkUpdateRequest) error {
	f.updateNetworkReq = req
	close(f.updateEntered)
	<-f.releaseUpdate
	return nil
}

func (f *serializedNetworkUpdateOps) Delete(sandbox.DeleteRequest) error {
	close(f.deleteEntered)
	return nil
}

func (f *rollbackNetworkUpdateOps) UpdateNetwork(_ context.Context, req sandbox.NetworkUpdateRequest) error {
	call := len(f.requests)
	f.requests = append(f.requests, req)
	if call < len(f.errs) {
		return f.errs[call]
	}
	return nil
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

func (f *fakeSandboxOps) UpdateNetwork(_ context.Context, req sandbox.NetworkUpdateRequest) error {
	f.updateNetworkReq = req
	return f.updateNetworkErr
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
	seedReadyTemplate(t, ctx, svc.Templates, "t0", "team-a", t0Digest, state.TemplateBootModeCold)

	before := state.SandboxRecord{
		PodSandboxID:                  "pod-a",
		ConchSandboxID:                "sandbox-a",
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
		Namespace:    "team-a",
		PodSandboxID: "pod-a",
		Labels:       map[string]string{"generation": "t1"},
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
	if t1.State != state.TemplateReady ||
		t1.BootIndexDigest != t1Digest || t1.BootMode != state.TemplateBootModeResume {
		t.Fatalf("t1 readiness = %#v", t1)
	}
	if t1.ParentTemplateID != "t0" || t1.SourceSandboxID != "sandbox-a" ||
		t1.BuildRef != "localhost/conch/checkpoints:t1" || t1.Labels["generation"] != "t1" {
		t.Fatalf("t1 lineage = %#v", t1)
	}

	after, err := store.GetSandbox(ctx, "pod-a")
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
	seedReadyTemplate(t, ctx, svc.Templates, "t0", "team-a", t0Digest, state.TemplateBootModeCold)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:                  "pod-lineage",
		ConchSandboxID:                "sandbox-lineage",
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

	t1Result, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{Namespace: "team-a", PodSandboxID: "pod-lineage"})
	if err != nil {
		t.Fatalf("CheckpointSandbox(t1) error = %v", err)
	}
	t2Result, err := svc.CheckpointSandbox(ctx, SandboxCheckpointOptions{Namespace: "team-a", PodSandboxID: "pod-lineage"})
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
	if t1.State != state.TemplateReady || t2.State != state.TemplateReady ||
		t1.BootIndexDigest != t1Digest || t2.BootIndexDigest != t2Digest {
		t.Fatalf("checkpoint template states = (%#v, %#v)", t1, t2)
	}

	rec, err := store.GetSandbox(ctx, "pod-lineage")
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

func TestSandboxLogsEmptyAndPerSandbox(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, nil, nil, nil, "default")

	empty, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{SandboxID: "sandbox-a"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	if len(empty.Logs) != 0 {
		t.Fatalf("empty logs = %#v, want none", empty.Logs)
	}

	svc.AppendSandboxLog("sandbox-a", "info", "created sandbox")
	svc.AppendSandboxLog("sandbox-b", "info", "other sandbox")
	svc.AppendSandboxLog("sandbox-a", "info", "paused sandbox")

	got, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{SandboxID: "sandbox-a"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"created sandbox", "paused sandbox"})
	if got.Logs[0].ID != 1 || got.Logs[1].ID != 2 {
		t.Fatalf("log IDs = %d,%d, want 1,2", got.Logs[0].ID, got.Logs[1].ID)
	}
	if got.Logs[0].SandboxID != "sandbox-a" || got.Logs[0].Level != "info" || got.Logs[0].Time.IsZero() {
		t.Fatalf("log entry = %#v, want structured sandbox info entry with timestamp", got.Logs[0])
	}
}

func TestSandboxLogBufferDropsOldestEntries(t *testing.T) {
	buf := newSandboxLogBuffer(2)
	buf.Append("sandbox-a", "info", "one")
	buf.Append("sandbox-a", "info", "two")
	buf.Append("sandbox-a", "info", "three")

	got := buf.Get(SandboxLogsOptions{SandboxID: "sandbox-a"})
	assertLogMessages(t, got.Logs, []string{"two", "three"})
	if got.Logs[0].ID != 2 || got.Logs[1].ID != 3 {
		t.Fatalf("log IDs = %d,%d, want 2,3", got.Logs[0].ID, got.Logs[1].ID)
	}
}

func TestSandboxLogCleanupLoopPrunesAndStops(t *testing.T) {
	logs := newSandboxLogBuffer(4, time.Millisecond)
	now := time.Unix(1, 0)
	logs.now = func() time.Time { return now }
	logs.Append("sandbox-a", "info", "expired")
	logs.Expire("sandbox-a")
	now = now.Add(2 * time.Millisecond)

	svc := &Service{SandboxLogs: logs}
	svc.StartSandboxLogCleanup(time.Millisecond)
	deadline := time.Now().Add(200 * time.Millisecond)
	key := normalizeSandboxLogKey(SandboxLogKey{SandboxID: "sandbox-a"})
	for {
		logs.mu.Lock()
		_, exists := logs.entries[key]
		logs.mu.Unlock()
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic cleanup did not prune expired logs")
		}
		time.Sleep(time.Millisecond)
	}

	svc.StopSandboxLogCleanup()
	stoppedLogs := newSandboxLogBuffer(4)
	stoppedService := &Service{SandboxLogs: stoppedLogs}
	stoppedService.StartSandboxLogCleanup(time.Millisecond)
	stoppedService.StopSandboxLogCleanup()
	key = normalizeSandboxLogKey(SandboxLogKey{SandboxID: "sandbox-b"})
	stoppedLogs.mu.Lock()
	stoppedLogs.entries[key] = []SandboxLogEntry{{SandboxID: "sandbox-b", Message: "retained"}}
	stoppedLogs.expiresAt[key] = time.Now().Add(-time.Hour)
	stoppedLogs.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	stoppedLogs.mu.Lock()
	_, exists := stoppedLogs.entries[key]
	stoppedLogs.mu.Unlock()
	if !exists {
		t.Fatal("cleanup loop continued after StopSandboxLogCleanup")
	}
}

func TestClearSandboxLogsResetsReusedSandboxID(t *testing.T) {
	svc := &Service{SandboxLogs: newSandboxLogBuffer(4)}
	svc.AppendSandboxLog("sandbox-a", "info", "old")
	svc.ClearSandboxLogs("sandbox-a")
	svc.AppendSandboxLog("sandbox-a", "info", "new")

	got, err := svc.GetSandboxLogs(context.Background(), SandboxLogsOptions{SandboxID: "sandbox-a"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	if len(got.Logs) != 1 || got.Logs[0].ID != 1 || got.Logs[0].Message != "new" {
		t.Fatalf("logs after ID reuse = %#v", got.Logs)
	}
}

func TestSandboxLogsCursorLimitLevelAndSearch(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, nil, nil, nil, "default")
	now := time.UnixMilli(1000)
	svc.SandboxLogs.now = func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	}
	svc.AppendSandboxLog("sandbox-a", "info", "created sandbox")
	svc.AppendSandboxLog("sandbox-a", "error", "network update failed")
	svc.AppendSandboxLog("sandbox-a", "warn", "network update delayed")
	svc.AppendSandboxLog("sandbox-a", "error", "delete failed")

	got, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{
		SandboxID: "sandbox-a",
		Cursor:    "1001:1",
		Limit:     1,
		Level:     "ERROR",
		Search:    "FAILED",
	})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"network update failed"})

	got, err = svc.GetSandboxLogs(ctx, SandboxLogsOptions{
		SandboxID: "sandbox-a",
		Cursor:    got.NextCursor,
		Level:     "error",
		Search:    "failed",
	})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"delete failed"})

	got, err = svc.GetSandboxLogs(ctx, SandboxLogsOptions{
		SandboxID: "sandbox-a",
		Cursor:    "1004:0",
		Limit:     2,
		Direction: "backward",
	})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"network update delayed", "network update failed"})
}

func TestSandboxLogsAreIsolatedByNamespace(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, nil, nil, nil, "default")
	svc.AppendSandboxLogFor("tenant-a", "sandbox-1", "info", "tenant a")
	svc.AppendSandboxLogFor("tenant-b", "sandbox-1", "info", "tenant b")

	got, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{Namespace: "tenant-a", SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"tenant a"})
	if got.Logs[0].Namespace != "tenant-a" {
		t.Fatalf("log namespace = %q, want tenant-a", got.Logs[0].Namespace)
	}
	svc.ClearSandboxLogsFor("tenant-a", "sandbox-1")
	got, err = svc.GetSandboxLogs(ctx, SandboxLogsOptions{Namespace: "tenant-b", SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"tenant b"})
}

func TestSandboxLogsCompoundCursorHandlesSameMillisecond(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, nil, nil, nil, "default")
	svc.SandboxLogs.now = func() time.Time { return time.UnixMilli(1000) }
	svc.AppendSandboxLog("sandbox-a", "info", "one")
	svc.AppendSandboxLog("sandbox-a", "info", "two")
	svc.AppendSandboxLog("sandbox-a", "info", "three")

	first, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{SandboxID: "sandbox-a", Limit: 2})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, first.Logs, []string{"one", "two"})
	if first.NextCursor != "1000:2" {
		t.Fatalf("NextCursor = %q, want 1000:2", first.NextCursor)
	}
	second, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{
		SandboxID: "sandbox-a",
		Cursor:    first.NextCursor,
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, second.Logs, []string{"three"})

	backward, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{
		SandboxID: "sandbox-a",
		Limit:     2,
		Direction: "backward",
	})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, backward.Logs, []string{"three", "two"})
	older, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{
		SandboxID: "sandbox-a",
		Cursor:    backward.NextCursor,
		Direction: "backward",
	})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, older.Logs, []string{"one"})
}

func TestRemoveSandboxExpiresLogsAfterTTL(t *testing.T) {
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
	svc := New(&fakeSandboxOps{}, nil, nil, store, "default")
	svc.SetSandboxLogTTL(time.Minute)
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	svc.SandboxLogs.now = func() time.Time { return now }
	svc.AppendSandboxLog("sandbox-1", "info", "created sandbox")

	if err := svc.RemoveSandbox(ctx, "default", "pod-1"); err != nil {
		t.Fatalf("RemoveSandbox() error = %v", err)
	}

	got, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"created sandbox", "deleted sandbox"})

	now = now.Add(time.Minute)
	got, err = svc.GetSandboxLogs(ctx, SandboxLogsOptions{SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	if len(got.Logs) != 0 {
		t.Fatalf("logs = %#v, want empty after TTL", got.Logs)
	}
}

func TestSandboxLogAppendBeforeTTLCancelsExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	svc := New(nil, nil, nil, nil, "default")
	svc.SetSandboxLogTTL(time.Minute)
	svc.SandboxLogs.now = func() time.Time { return now }
	svc.AppendSandboxLog("sandbox-a", "info", "created sandbox")
	svc.ExpireSandboxLogs("sandbox-a")

	now = now.Add(30 * time.Second)
	svc.AppendSandboxLog("sandbox-a", "info", "reused sandbox")

	now = now.Add(time.Minute)
	got, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{SandboxID: "sandbox-a"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"created sandbox", "reused sandbox"})
}

func TestSuspendSandboxExpiresLogsAfterTTL(t *testing.T) {
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
	svc := New(&fakeSandboxOps{}, nil, nil, store, "default")
	svc.SetSandboxLogTTL(time.Minute)
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	svc.SandboxLogs.now = func() time.Time { return now }
	svc.AppendSandboxLog("sandbox-1", "info", "created sandbox")

	if err := svc.SuspendSandbox(ctx, "default", "pod-1"); err != nil {
		t.Fatalf("SuspendSandbox() error = %v", err)
	}
	got, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"created sandbox", "paused sandbox"})

	now = now.Add(time.Minute)
	got, err = svc.GetSandboxLogs(ctx, SandboxLogsOptions{SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	if len(got.Logs) != 0 {
		t.Fatalf("logs = %#v, want empty after TTL", got.Logs)
	}
}

func TestResumeSandboxCancelsLogExpiry(t *testing.T) {
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
	svc := New(&fakeSandboxOps{}, nil, nil, store, "default")
	svc.SetSandboxLogTTL(time.Minute)
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	svc.SandboxLogs.now = func() time.Time { return now }
	svc.AppendSandboxLog("sandbox-1", "info", "created sandbox")

	if err := svc.SuspendSandbox(ctx, "default", "pod-1"); err != nil {
		t.Fatalf("SuspendSandbox() error = %v", err)
	}
	now = now.Add(30 * time.Second)
	if err := svc.ResumeSandbox(ctx, "default", "pod-1"); err != nil {
		t.Fatalf("ResumeSandbox() error = %v", err)
	}

	now = now.Add(time.Minute)
	got, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("GetSandboxLogs() error = %v", err)
	}
	assertLogMessages(t, got.Logs, []string{"created sandbox", "paused sandbox", "resumed sandbox"})
}

func assertLogMessages(t *testing.T, got []SandboxLogEntry, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("logs = %#v, want messages %#v", got, want)
	}
	for i := range want {
		if got[i].Message != want[i] {
			t.Fatalf("logs[%d].Message = %q, want %q", i, got[i].Message, want[i])
		}
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
	svc := New(sandboxOps, nil, nil, store, "default")
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

func TestRemoveSandboxDoesNotCreateStateForUnknownRuntime(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	sandboxOps := &fakeSandboxOps{deleteErr: errors.New("sandbox not found")}
	svc := New(sandboxOps, nil, nil, store, "default")
	if err := svc.RemoveSandbox(ctx, "default", "missing-pod"); err != nil {
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
		PodSandboxID:   "pod-serialized",
		ConchSandboxID: "sandbox-serialized",
		Namespace:      "default",
		State:          state.SandboxReady,
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
	go func() { firstDone <- svc.RemoveSandbox(ctx, "default", rec.PodSandboxID) }()
	<-ops.firstEntered
	go func() { secondDone <- svc.RemoveSandbox(ctx, "default", rec.PodSandboxID) }()
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
	if _, err := store.GetSandbox(ctx, rec.PodSandboxID); !errors.Is(err, state.ErrNotFound) {
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
		PodSandboxID: "pod-1",
		SandboxID:    "sandbox-1",
		TemplateID:   "tmpl-1",
	}); err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	rec, err := store.GetSandbox(ctx, "pod-1")
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

func TestCreateSandboxForwardsAndPersistsNetwork(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil, store, "default")
	allowInternet := false
	network := &runtimeapi.SandboxNetworkConfig{
		AllowOut:            []string{"192.0.2.1"},
		DenyOut:             []string{"198.51.100.0/24"},
		AllowInternetAccess: &allowInternet,
	}

	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		PodSandboxID: "pod-1",
		SandboxID:    "sandbox-1",
		TemplateID:   "tmpl-1",
		Network:      network,
	}); err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	var forwarded runtimeapi.SandboxNetworkConfig
	if err := json.Unmarshal(sandboxOps.req.Network, &forwarded); err != nil {
		t.Fatalf("decode forwarded network: %v", err)
	}
	rec, err := store.GetSandbox(ctx, "pod-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	var persisted runtimeapi.SandboxNetworkConfig
	if err := json.Unmarshal(rec.Network, &persisted); err != nil {
		t.Fatalf("decode persisted network: %v", err)
	}
	if !reflect.DeepEqual(forwarded, *network) || !reflect.DeepEqual(persisted, *network) {
		t.Fatalf("network forwarding mismatch: forwarded=%#v persisted=%#v", forwarded, persisted)
	}
}

func TestUpdateSandboxNetworkConfigReplacesEgressAndPreservesIngress(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	allowPublicTraffic := true
	maskRequestHost := "masked.example"
	allowInternet := false
	existing, err := json.Marshal(runtimeapi.SandboxNetworkConfig{
		AllowPublicTraffic:  &allowPublicTraffic,
		AllowOut:            []string{"192.0.2.1"},
		DenyOut:             []string{"198.51.100.0/24"},
		EgressProxy:         &runtimeapi.SandboxEgressProxyConfig{Address: "http://proxy.example"},
		MaskRequestHost:     &maskRequestHost,
		Rules:               map[string]any{"rule": "value"},
		AllowInternetAccess: &allowInternet,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "default",
		State:          state.SandboxReady,
		Network:        existing,
	}); err != nil {
		t.Fatal(err)
	}
	sandboxOps := &fakeSandboxOps{}
	svc := New(sandboxOps, nil, nil, store, "default")
	denyOut := []string{"203.0.113.0/24"}
	if err := svc.UpdateSandboxNetworkConfig(ctx, SandboxNetworkUpdateOptions{
		PodSandboxID: "pod-1",
		SandboxID:    "sandbox-1",
		Network: &runtimeapi.SandboxNetworkUpdateConfig{
			DenyOut: &denyOut,
		},
	}); err != nil {
		t.Fatalf("UpdateSandboxNetworkConfig() error = %v", err)
	}

	var got runtimeapi.SandboxNetworkConfig
	if err := json.Unmarshal(sandboxOps.updateNetworkReq.Network, &got); err != nil {
		t.Fatalf("decode update request: %v", err)
	}
	if got.AllowPublicTraffic == nil || !*got.AllowPublicTraffic ||
		got.MaskRequestHost == nil || *got.MaskRequestHost != maskRequestHost ||
		!reflect.DeepEqual(got.DenyOut, denyOut) ||
		len(got.AllowOut) != 0 || got.EgressProxy != nil || len(got.Rules) != 0 ||
		got.AllowInternetAccess != nil {
		t.Fatalf("resolved network = %#v", got)
	}
	rec, err := store.GetSandbox(ctx, "pod-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.Network) != string(sandboxOps.updateNetworkReq.Network) {
		t.Fatalf("persisted network = %s, applied network = %s", rec.Network, sandboxOps.updateNetworkReq.Network)
	}
	logs, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{Namespace: "default", SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatal(err)
	}
	assertLogMessages(t, logs.Logs, []string{"network policy updated"})
}

func TestUpdateSandboxNetworkConfigRollsBackInvalidPolicy(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	existing, err := json.Marshal(runtimeapi.SandboxNetworkConfig{AllowOut: []string{"192.0.2.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "default",
		State:          state.SandboxReady,
		Network:        existing,
	}); err != nil {
		t.Fatal(err)
	}
	sandboxOps := &fakeSandboxOps{updateNetworkErr: netstack.ErrInvalidSandboxNetworkPolicy}
	svc := New(sandboxOps, nil, nil, store, "default")
	invalid := []string{"example.com"}
	err = svc.UpdateSandboxNetworkConfig(ctx, SandboxNetworkUpdateOptions{
		PodSandboxID: "pod-1",
		SandboxID:    "sandbox-1",
		Network:      &runtimeapi.SandboxNetworkUpdateConfig{AllowOut: &invalid},
	})
	if !errors.Is(err, netstack.ErrInvalidSandboxNetworkPolicy) {
		t.Fatalf("UpdateSandboxNetworkConfig() error = %v", err)
	}
	rec, err := store.GetSandbox(ctx, "pod-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.Network) != string(existing) {
		t.Fatalf("Network = %s, want %s", rec.Network, existing)
	}
	if rec.LastError == "" {
		t.Fatal("LastError is empty")
	}
	logs, err := svc.GetSandboxLogs(ctx, SandboxLogsOptions{Namespace: "default", SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatal(err)
	}
	assertLogMessages(t, logs.Logs, []string{"network policy update failed"})
}

func TestUpdateSandboxNetworkConfigRestoresPolicyAfterApplyFailure(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	existing, err := json.Marshal(runtimeapi.SandboxNetworkConfig{
		AllowOut: []string{"192.0.2.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "default",
		State:          state.SandboxReady,
		Network:        existing,
	}); err != nil {
		t.Fatal(err)
	}
	applyErr := errors.New("partial iptables update")
	sandboxOps := &rollbackNetworkUpdateOps{errs: []error{applyErr, nil}}
	svc := New(sandboxOps, nil, nil, store, "default")
	denyOut := []string{"198.51.100.1"}

	err = svc.UpdateSandboxNetworkConfig(ctx, SandboxNetworkUpdateOptions{
		PodSandboxID: "pod-1",
		SandboxID:    "sandbox-1",
		Network:      &runtimeapi.SandboxNetworkUpdateConfig{DenyOut: &denyOut},
	})
	if !errors.Is(err, applyErr) {
		t.Fatalf("UpdateSandboxNetworkConfig() error = %v, want apply failure", err)
	}
	if len(sandboxOps.requests) != 2 {
		t.Fatalf("UpdateNetwork calls = %d, want apply and rollback", len(sandboxOps.requests))
	}
	if string(sandboxOps.requests[1].Network) != string(existing) {
		t.Fatalf("rollback network = %s, want %s", sandboxOps.requests[1].Network, existing)
	}
	rec, err := store.GetSandbox(ctx, "pod-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.Network) != string(existing) {
		t.Fatalf("persisted network = %s, want restored %s", rec.Network, existing)
	}
}

func TestUpdateSandboxNetworkConfigMarksUnknownWhenRollbackFails(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "default",
		State:          state.SandboxReady,
	}); err != nil {
		t.Fatal(err)
	}
	applyErr := errors.New("apply failed")
	rollbackErr := errors.New("rollback failed")
	sandboxOps := &rollbackNetworkUpdateOps{errs: []error{applyErr, rollbackErr}}
	svc := New(sandboxOps, nil, nil, store, "default")

	err := svc.UpdateSandboxNetworkConfig(ctx, SandboxNetworkUpdateOptions{
		PodSandboxID: "pod-1",
		SandboxID:    "sandbox-1",
		Network:      &runtimeapi.SandboxNetworkUpdateConfig{},
	})
	if !errors.Is(err, applyErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("UpdateSandboxNetworkConfig() error = %v, want apply and rollback failures", err)
	}
	rec, err := store.GetSandbox(ctx, "pod-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != state.SandboxUnknown {
		t.Fatalf("sandbox state = %q, want %q", rec.State, state.SandboxUnknown)
	}
}

func TestUpdateSandboxNetworkConfigSerializesWithDelete(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "sandbox-1",
		Namespace:      "default",
		State:          state.SandboxReady,
	}); err != nil {
		t.Fatal(err)
	}
	sandboxOps := &serializedNetworkUpdateOps{
		updateEntered: make(chan struct{}),
		releaseUpdate: make(chan struct{}),
		deleteEntered: make(chan struct{}),
	}
	defer func() {
		select {
		case <-sandboxOps.releaseUpdate:
		default:
			close(sandboxOps.releaseUpdate)
		}
	}()
	svc := New(sandboxOps, nil, nil, store, "default")
	denyOut := []string{"198.51.100.1"}
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- svc.UpdateSandboxNetworkConfig(ctx, SandboxNetworkUpdateOptions{
			PodSandboxID: "pod-1",
			SandboxID:    "sandbox-1",
			Network:      &runtimeapi.SandboxNetworkUpdateConfig{DenyOut: &denyOut},
		})
	}()
	<-sandboxOps.updateEntered

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- svc.RemoveSandbox(ctx, "default", "pod-1")
	}()
	select {
	case <-sandboxOps.deleteEntered:
		t.Fatal("delete entered sandbox backend while network update held lifecycle lock")
	case <-time.After(50 * time.Millisecond):
	}

	close(sandboxOps.releaseUpdate)
	if err := <-updateDone; err != nil {
		t.Fatalf("UpdateSandboxNetworkConfig() error = %v", err)
	}
	select {
	case <-sandboxOps.deleteEntered:
	case <-time.After(time.Second):
		t.Fatal("delete did not proceed after network update released lifecycle lock")
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("RemoveSandbox() error = %v", err)
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

func seedReadyTemplate(t *testing.T, ctx context.Context, templates conchtemplate.Store, id, namespace, bootIndexDigest, bootMode string) {
	t.Helper()
	if _, err := templates.Create(ctx, conchtemplate.CreateRequest{
		ID:        id,
		Origin:    state.TemplateOriginImage,
		Namespace: namespace,
	}); err != nil {
		t.Fatalf("CreateTemplate(%s) error = %v", id, err)
	}
	if err := templates.MarkReady(ctx, id, conchtemplate.ReadyState{
		BootIndexDigest: bootIndexDigest,
		BootMode:        bootMode,
		BuildRef:        "localhost/conch/templates:" + id,
	}); err != nil {
		t.Fatalf("MarkReady(%s) error = %v", id, err)
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
