package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/apperror"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/vmm"
)

// pauseFixture builds a READY E2B sandbox whose checkpoint head is a real boot
// index in the content store, so Pause runs the full beginCheckpoint pipeline.
func pauseFixture(t *testing.T, boot *recordingBootPreparer) (
	*Manager, *memorySandboxStore, *testLeases, *recordingCheckpointCapture, *fakeRouteTable, *fakeCapacityLedger, Record,
) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required for checkpoint content")
	}
	m, store, leaseStore := newLifecycleTestManager(t, boot)
	components := make([]ocispec.Descriptor, 0, 2)
	for _, kind := range []string{conchimage.KindRootfs, conchimage.KindSandbox} {
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
		RootfsDescriptor: components[0], SandboxDescriptor: components[1],
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
		MaskRequestHost: "app-${PORT}.internal", Metadata: map[string]string{"team": "core"},
		RuntimeSnapshots: []SnapshotRef{
			{Snapshotter: "erofs", Role: "rootfs", Key: req.SandboxID},
			{Snapshotter: "erofs", Role: "vm", Key: "view-vm-" + req.SandboxID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mem.img"), []byte("memory"), 0600); err != nil {
		t.Fatal(err)
	}
	capture := &recordingCheckpointCapture{result: CapturedBootComponents{MemRootPath: dir, VMMName: "cloud-hypervisor", MemorySizeMB: 512}}
	m.checkpointCapture = capture
	routes, capacity := newFakeRouteTable(), &fakeCapacityLedger{}
	m.Routes, m.Capacity, m.Envd = routes, capacity, &fakeEnvdBootstrapper{version: "1.2.3"}
	// Mirror the create-time side effects the seeded record stands for.
	if err := capacity.Reserve(req.SandboxID, req.VCPUNum, req.RAMMB); err != nil {
		t.Fatal(err)
	}
	routes.Insert(req.SandboxID)
	m.sandboxes.Store(req.SandboxID, &sandboxEntry{state: StateReady, sbx: &Sandbox{process: &vmm.Process{}}})
	return m, store, leaseStore, capture, routes, capacity, rec
}

func TestPauseCheckpointsAndReleasesRuntime(t *testing.T) {
	m, store, leaseStore, capture, routes, capacity, original := pauseFixture(t, readyBoot())
	ctx := context.Background()

	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StatePaused || rec.LastError != "" || rec.IP != "" || rec.VMMPID != 0 || rec.ExpiresAt != 0 {
		t.Fatalf("paused record = %#v", rec)
	}
	// The paused record pins only the head boot index; the run's active/view
	// snapshot refs went with the released runtime.
	if len(rec.RuntimeSnapshots) != 0 {
		t.Fatalf("paused record kept runtime snapshots: %#v", rec.RuntimeSnapshots)
	}
	if rec.CheckpointHeadTemplateID == original.CheckpointHeadTemplateID {
		t.Fatalf("checkpoint head did not move: %#v", rec)
	}
	if rec.HeadStartedAt != original.StartedAt || rec.HeadCreatedAt == 0 || rec.HeadCreatedAt == original.HeadCreatedAt {
		t.Fatalf("head run timestamps = %d, %d", rec.HeadStartedAt, rec.HeadCreatedAt)
	}
	if rec.StartedAt != original.StartedAt {
		t.Fatalf("pause rewrote the run start: %#v", rec)
	}
	if len(capture.requests) != 1 || !capture.requests[0].PauseBefore {
		t.Fatalf("capture requests = %#v", capture.requests)
	}
	// Pause hands the VM over suspended straight into the stop phase.
	if !capture.requests[0].LeavePaused {
		t.Fatalf("capture requests = %#v, want LeavePaused", capture.requests)
	}
	if _, err := os.Stat(capture.result.MemRootPath); !os.IsNotExist(err) {
		t.Fatalf("capture directory remains: %v", err)
	}
	if remaining, _ := leaseStore.List(ctx); len(remaining) != 0 {
		t.Fatalf("content leases remain: %#v", remaining)
	}
	if _, ok := capacity.holding("sandbox-a"); ok || capacity.releaseCount("sandbox-a") != 1 {
		t.Fatal("paused sandbox kept or leaked the capacity reservation")
	}
	if routes.releaseCount("sandbox-a") != 1 {
		t.Fatalf("route releases = %d", routes.releaseCount("sandbox-a"))
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("runtime entry remains after pause")
	}
}

// TestPauseLabelsCheckpointChainForGC pins the pause checkpoint's GC
// ownership: the record pins only the head digest, so Pause must chain the
// head index to its manifests, configs and layers with
// containerd.io/gc.ref.content labels — without them the released lease
// leaves the checkpoint content unreachable and GC deletes it.
func TestPauseLabelsCheckpointChainForGC(t *testing.T) {
	m, store, _, _, _, _, _ := pauseFixture(t, readyBoot())
	ctx := context.Background()

	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	cs := m.client.ContentStore()

	indexInfo, err := cs.Info(ctx, digest.Digest(rec.CheckpointHeadTemplateID))
	if err != nil {
		t.Fatalf("head index Info() error = %v", err)
	}
	raw, err := content.ReadBlob(ctx, cs, ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex, Digest: indexInfo.Digest, Size: indexInfo.Size,
	})
	if err != nil {
		t.Fatalf("read head index: %v", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("unmarshal head index: %v", err)
	}
	if len(index.Manifests) != 3 {
		t.Fatalf("checkpoint index manifests = %d, want 3 (rootfs, mem, sandbox)", len(index.Manifests))
	}
	for i, manifest := range index.Manifests {
		if got := indexInfo.Labels[fmt.Sprintf("containerd.io/gc.ref.content.m.%d", i)]; got != manifest.Digest.String() {
			t.Fatalf("head index label m.%d = %q, want %s", i, got, manifest.Digest)
		}
	}

	sawMem := false
	for _, manifestDesc := range index.Manifests {
		if manifestDesc.Annotations["io.conch.kind"] == conchimage.KindMemSnapshot {
			sawMem = true
		}
		manifestInfo, err := cs.Info(ctx, manifestDesc.Digest)
		if err != nil {
			t.Fatalf("manifest %s Info() error = %v", manifestDesc.Digest, err)
		}
		rawManifest, err := content.ReadBlob(ctx, cs, manifestDesc)
		if err != nil {
			t.Fatalf("read manifest %s: %v", manifestDesc.Digest, err)
		}
		var manifest ocispec.Manifest
		if err := json.Unmarshal(rawManifest, &manifest); err != nil {
			t.Fatalf("unmarshal manifest %s: %v", manifestDesc.Digest, err)
		}
		if got := manifestInfo.Labels["containerd.io/gc.ref.content.config"]; got != manifest.Config.Digest.String() {
			t.Fatalf("manifest %s config label = %q, want %s", manifestDesc.Digest, got, manifest.Config.Digest)
		}
		for i, layer := range manifest.Layers {
			if got := manifestInfo.Labels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)]; got != layer.Digest.String() {
				t.Fatalf("manifest %s layer label l.%d = %q, want %s", manifestDesc.Digest, i, got, layer.Digest)
			}
		}
	}
	if !sawMem {
		t.Fatal("checkpoint index has no mem-snapshot manifest")
	}
}

func TestPauseRejectsInvalidStatesAndStaleExpiry(t *testing.T) {
	m, store, _, _, _, capacity, _ := pauseFixture(t, readyBoot())
	// Replace the fixture sandbox with records that fail before capture.
	m.sandboxes.Delete("sandbox-a")
	capture := &recordingCheckpointCapture{}
	m.checkpointCapture = capture
	ctx := context.Background()
	base := testCreateRequest()
	future := time.Now().Add(time.Hour).UnixNano()
	for _, tc := range []struct {
		name          string
		record        Record
		expiredBefore time.Time
	}{
		{"native sandbox", Record{ID: "native", State: StateReady, E2B: false, CheckpointHeadTemplateID: base.TemplateID}, time.Time{}},
		{"already paused", Record{ID: "paused", State: StatePaused, E2B: true, CheckpointHeadTemplateID: base.TemplateID}, time.Time{}},
		{"creating", Record{ID: "creating", State: StateCreating, E2B: true, SourceTemplateID: base.TemplateID}, time.Time{}},
		{"suspended", Record{ID: "suspended", State: StateSuspended, E2B: true, CheckpointHeadTemplateID: base.TemplateID}, time.Time{}},
		{"no runtime entry", Record{ID: "entryless", State: StateReady, E2B: true, CheckpointHeadTemplateID: base.TemplateID}, time.Time{}},
		{"not expired yet", Record{ID: "live", State: StateReady, E2B: true, CheckpointHeadTemplateID: base.TemplateID, ExpiresAt: future}, time.Now()},
		{"no deadline", Record{ID: "nodeadline", State: StateReady, E2B: true, CheckpointHeadTemplateID: base.TemplateID}, time.Now()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Create(ctx, tc.record); err != nil {
				t.Fatal(err)
			}
			if err := m.Pause(ctx, tc.record.ID, tc.expiredBefore); err == nil {
				t.Fatal("invalid pause accepted")
			}
		})
	}
	if err := m.Pause(ctx, "missing", time.Time{}); err == nil {
		t.Fatal("unknown sandbox accepted")
	}
	if len(capture.requests) != 0 || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatalf("rejected pause captured or released: %#v", capture.requests)
	}
}

// TestPauseDeadRuntimeReturnsSentinel pins the refusal path for a runtime
// degraded past Ready/Suspended: Pause answers with the distinct
// ErrRuntimeDead sentinel — still a public precondition — without entering
// the capture, touching the record, or releasing anything.
func TestPauseDeadRuntimeReturnsSentinel(t *testing.T) {
	m, store, _, capture, routes, capacity, original := pauseFixture(t, readyBoot())
	ctx := context.Background()
	value, _ := m.sandboxes.Load("sandbox-a")
	entry := value.(*sandboxEntry)
	entry.state = StateUnknown

	err := m.Pause(ctx, "sandbox-a", time.Time{})
	if !errors.Is(err, ErrRuntimeDead) {
		t.Fatalf("Pause() error = %v, want ErrRuntimeDead", err)
	}
	if errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("Pause() error = %v, collapsed into the generic precondition", err)
	}
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Kind() != apperror.FailedPrecondition {
		t.Fatalf("Pause() error = %v, want a public precondition", err)
	}
	rec, getErr := store.Get(ctx, "sandbox-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if rec.State != StateReady || rec.LastError != "" || rec.ExpiresAt != original.ExpiresAt ||
		rec.CheckpointHeadTemplateID != original.CheckpointHeadTemplateID {
		t.Fatalf("refused pause mutated the record: %#v", rec)
	}
	if len(capture.requests) != 0 {
		t.Fatalf("dead runtime was captured: %#v", capture.requests)
	}
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("refused pause dropped the capacity reservation")
	}
	if routes.releaseCount("sandbox-a") != 0 {
		t.Fatal("refused pause released the proxy route")
	}
	if value, ok := m.sandboxes.Load("sandbox-a"); !ok || value.(*sandboxEntry) != entry || entry.state != StateUnknown {
		t.Fatal("refused pause dropped or restored the runtime entry")
	}
}

// TestPauseDeadRuntimeWithoutEntry pins the entry-less shape of a crashed
// READY record — the crash rule deleted its runtime entry but kept the record
// READY — as the same ErrRuntimeDead sentinel, which the expiry dispatcher's
// delete fallback keys on.
func TestPauseDeadRuntimeWithoutEntry(t *testing.T) {
	m, store, _, capture, _, capacity, original := pauseFixture(t, readyBoot())
	ctx := context.Background()
	m.sandboxes.Delete("sandbox-a")

	err := m.Pause(ctx, "sandbox-a", time.Time{})
	if !errors.Is(err, ErrRuntimeDead) {
		t.Fatalf("Pause() error = %v, want ErrRuntimeDead", err)
	}
	rec, getErr := store.Get(ctx, "sandbox-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if rec.State != StateReady || rec.ExpiresAt != original.ExpiresAt {
		t.Fatalf("refused pause mutated the record: %#v", rec)
	}
	if len(capture.requests) != 0 {
		t.Fatalf("dead runtime was captured: %#v", capture.requests)
	}
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("refused pause dropped the capacity reservation")
	}
}

func TestPauseCleanupFailureRetainsUnknownUntilDelete(t *testing.T) {
	b := readyBoot()
	b.releaseErr = errors.New("boot release failed")
	m, store, _, capture, routes, capacity, original := pauseFixture(t, b)
	ctx := context.Background()

	err := m.Pause(ctx, "sandbox-a", time.Time{})
	if err == nil {
		t.Fatal("pause with failing cleanup succeeded")
	}
	rec, getErr := store.Get(ctx, "sandbox-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if rec.State != StateUnknown || rec.LastError == "" {
		t.Fatalf("retained record = %#v", rec)
	}
	if rec.CheckpointHeadTemplateID == original.CheckpointHeadTemplateID || rec.HeadCreatedAt == 0 {
		t.Fatalf("retained record lost the pause checkpoint: %#v", rec)
	}
	// The unconfirmed release keeps the reservation for the explicit delete.
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("unconfirmed pause cleanup dropped the capacity reservation")
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); !ok {
		t.Fatal("unconfirmed pause cleanup dropped the runtime entry")
	}
	if routes.releaseCount("sandbox-a") != 1 {
		t.Fatalf("route releases = %d", routes.releaseCount("sandbox-a"))
	}
	if _, err := os.Stat(capture.result.MemRootPath); !os.IsNotExist(err) {
		t.Fatalf("capture directory remains: %v", err)
	}

	// Nothing retries the retained record; an explicit Delete finishes it.
	b.releaseErr = nil
	if err := m.Delete(ctx, "sandbox-a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, "sandbox-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retained record = %v", err)
	}
	if _, ok := capacity.holding("sandbox-a"); ok || capacity.releaseCount("sandbox-a") != 1 {
		t.Fatal("explicit delete kept or leaked the capacity reservation")
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("entry remains after the explicit delete")
	}
}

// vmmResumeEntry builds a sandbox entry whose VMM resume answers through a
// local control socket; no VM is booted. The returned counter tallies the
// control requests served — one per Resume call.
func vmmResumeEntry(t *testing.T, status *atomic.Int32) (*sandboxEntry, *atomic.Int32) {
	t.Helper()
	payload := filepath.Join(t.TempDir(), "kernel")
	if err := os.WriteFile(payload, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	args := &vmm.ResourceArgs{KernelPath: payload, InitrdPath: payload}
	process, err := vmm.NewProcess("cloud-hypervisor", "/bin/true", "sandbox-a", args, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Stop(context.Background()) })
	fd, err := unix.Dup(args.ApiSocketFd)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "vmm-api")
	listener, err := net.FileListener(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	hits := &atomic.Int32{}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Connection", "close")
		w.WriteHeader(int(status.Load()))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	return &sandboxEntry{state: StateReady, sbx: &Sandbox{process: process}}, hits
}

// vmmStopFailureEntry builds a started VMM process whose control socket is
// served locally: the delete endpoint answers deleteStatus, the resume
// endpoint resumeStatus. ignoreTerm keeps the process alive through SIGTERM —
// held on a never-served fifo so it never forks — making Stop fail on its
// timeout with the VM still suspended and its control socket still in place.
// The counters tally the delete and resume requests.
func vmmStopFailureEntry(t *testing.T, ignoreTerm bool, deleteStatus, resumeStatus int) (*sandboxEntry, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexec sleep 60\n"
	if ignoreTerm {
		fifo := filepath.Join(dir, "hold.fifo")
		if err := unix.Mkfifo(fifo, 0600); err != nil {
			t.Fatal(err)
		}
		script = "#!/bin/sh\ntrap '' TERM\nread x < \"" + fifo + "\"\n"
	}
	scriptPath := filepath.Join(dir, "vmm.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, "kernel")
	if err := os.WriteFile(payload, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	args := &vmm.ResourceArgs{KernelPath: payload, InitrdPath: payload, NetNSPath: "/proc/self/ns/net"}
	process, err := vmm.NewProcess("cloud-hypervisor", scriptPath, "sandbox-a", args, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(process.Pid(), unix.SIGKILL) })
	// The booted event is buffered into the event socket before Create closes
	// the parent's copy of the child side.
	if _, err := unix.Write(args.EventMonitorFd, []byte(`{"source":"vm","event":"booted"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Dup(args.ApiSocketFd)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "vmm-api")
	listener, err := net.FileListener(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	deleteHits, resumeHits := &atomic.Int32{}, &atomic.Int32{}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vm.delete":
			deleteHits.Add(1)
			w.WriteHeader(deleteStatus)
		case "/api/v1/vm.resume":
			resumeHits.Add(1)
			w.WriteHeader(resumeStatus)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	createCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := process.Create(createCtx); err != nil {
		t.Fatal(err)
	}
	return &sandboxEntry{state: StateReady, sbx: &Sandbox{process: process}}, resumeHits, deleteHits
}

func TestPauseCheckpointResumeFailureUndoesSuspension(t *testing.T) {
	m, store, _, _, routes, capacity, _ := pauseFixture(t, readyBoot())
	status := &atomic.Int32{}
	status.Store(http.StatusNoContent)
	resumeEntry, _ := vmmResumeEntry(t, status)
	m.sandboxes.Store("sandbox-a", resumeEntry)
	resumeErr := errors.New("resume failed")
	m.checkpointCapture.(*recordingCheckpointCapture).err = errors.Join(ErrCheckpointResume, resumeErr)
	ctx := context.Background()

	err := m.Pause(ctx, "sandbox-a", time.Time{})
	if !errors.Is(err, ErrCheckpointResume) || !errors.Is(err, resumeErr) {
		t.Fatalf("Pause() error = %v, want joined resume failure", err)
	}
	rec, getErr := store.Get(ctx, "sandbox-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if rec.State != StateReady || rec.LastError != "" {
		t.Fatalf("undo did not restore the record: %#v", rec)
	}
	entry, _ := m.sandboxes.Load("sandbox-a")
	if entry.(*sandboxEntry).state != StateReady {
		t.Fatalf("undo did not restore the runtime: %s", entry.(*sandboxEntry).state)
	}
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("undo released the capacity reservation")
	}
	if routes.releaseCount("sandbox-a") != 0 {
		t.Fatal("undo released the proxy route")
	}

	// A failing VMM resume degrades both sides to UNKNOWN.
	status.Store(http.StatusInternalServerError)
	if err := m.Pause(ctx, "sandbox-a", time.Time{}); !errors.Is(err, ErrCheckpointResume) {
		t.Fatalf("second Pause() error = %v", err)
	}
	rec, err = store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateUnknown || rec.LastError == "" {
		t.Fatalf("degraded record = %#v", rec)
	}
	if entry, _ := m.sandboxes.Load("sandbox-a"); entry.(*sandboxEntry).state != StateUnknown {
		t.Fatalf("degraded runtime = %s", entry.(*sandboxEntry).state)
	}
}

// errLabelWalk fails the pause label walk while the VM is still suspended;
// errPausedWrite fails the PAUSED record commit after the VM already stopped.
var (
	errLabelWalk   = errors.New("label store down")
	errPausedWrite = errors.New("paused write failed")
)

// labelFailingContentStore breaks the gc.ref label updates of the pause label
// walk while delegating everything else, so the failure lands after a
// successful publish.
type labelFailingContentStore struct {
	content.Store
}

func (s *labelFailingContentStore) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
	for _, path := range fieldpaths {
		if strings.HasPrefix(path, "labels.containerd.io/gc.ref.content") {
			return content.Info{}, errLabelWalk
		}
	}
	return s.Store.Update(ctx, info, fieldpaths...)
}

// TestPauseResumesSandboxWhenCommitFails pins the recovery window before the
// VMM dies: a publish failure inside beginCheckpoint, a label-walk failure
// and a VMM stop failure all hand the sandbox back running with its READY
// record untouched, instead of leaving the guest suspended behind a running
// facade.
func TestPauseResumesSandboxWhenCommitFails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stopFailure bool
		setup       func(t *testing.T, m *Manager, store *memorySandboxStore, leaseStore *testLeases, capture *recordingCheckpointCapture)
		want        error
	}{
		{
			name: "publish failure after capture",
			setup: func(t *testing.T, _ *Manager, _ *memorySandboxStore, _ *testLeases, capture *recordingCheckpointCapture) {
				capture.result.MemRootPath = filepath.Join(t.TempDir(), "missing")
			},
			want: ErrCheckpointCaptured,
		},
		{
			name: "label walk failure",
			setup: func(t *testing.T, m *Manager, _ *memorySandboxStore, leaseStore *testLeases, _ *recordingCheckpointCapture) {
				client, err := containerd.New("", containerd.WithServices(
					containerd.WithLeasesService(leaseStore),
					containerd.WithContentStore(&labelFailingContentStore{Store: m.client.ContentStore()}),
				))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = client.Close() })
				m.client = &containerdclient.Client{Client: client}
			},
			want: errLabelWalk,
		},
		{
			// The VM survives the stop (SIGTERM ignored) and stays suspended;
			// the recovery resume hands it back running.
			name:        "VMM stop failure",
			stopFailure: true,
			want:        context.DeadlineExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, store, leaseStore, capture, routes, capacity, original := pauseFixture(t, readyBoot())
			var entry *sandboxEntry
			var resumeHits, deleteHits *atomic.Int32
			if tc.stopFailure {
				entry, resumeHits, deleteHits = vmmStopFailureEntry(t, true, http.StatusNoContent, http.StatusNoContent)
			} else {
				status := &atomic.Int32{}
				status.Store(http.StatusNoContent)
				entry, resumeHits = vmmResumeEntry(t, status)
			}
			m.sandboxes.Store("sandbox-a", entry)
			if tc.setup != nil {
				tc.setup(t, m, store, leaseStore, capture)
			}
			ctx := context.Background()

			err := m.Pause(ctx, "sandbox-a", time.Time{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Pause() error = %v, want %v in the chain", err, tc.want)
			}
			if errors.Is(err, ErrCheckpointResume) {
				t.Fatalf("Pause() error = %v, misrouted to the capture-resume branch", err)
			}
			if strings.Contains(err.Error(), "failed to stop VMM") != tc.stopFailure {
				t.Fatalf("Pause() error = %v, stop-failure marking mismatch", err)
			}
			if resumeHits.Load() != 1 {
				t.Fatalf("resume requests = %d, want exactly one", resumeHits.Load())
			}
			if deleteHits != nil && deleteHits.Load() != 1 {
				t.Fatalf("delete requests = %d, want exactly one", deleteHits.Load())
			}
			if len(capture.requests) != 1 || !capture.requests[0].LeavePaused {
				t.Fatalf("capture requests = %#v, want one suspended handover", capture.requests)
			}
			rec, getErr := store.Get(ctx, "sandbox-a")
			if getErr != nil {
				t.Fatal(getErr)
			}
			if rec.State != StateReady || rec.LastError != "" || rec.ExpiresAt != original.ExpiresAt ||
				rec.IP != original.IP || rec.VMMPID != original.VMMPID ||
				rec.CheckpointHeadTemplateID != original.CheckpointHeadTemplateID ||
				len(rec.RuntimeSnapshots) != len(original.RuntimeSnapshots) {
				t.Fatalf("recovered record lost the running view: %#v", rec)
			}
			if value, ok := m.sandboxes.Load("sandbox-a"); !ok || value.(*sandboxEntry).state != StateReady {
				t.Fatal("recovered runtime entry missing or not ready")
			}
			if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
				t.Fatal("recovery released the capacity reservation")
			}
			if routes.releaseCount("sandbox-a") != 0 {
				t.Fatal("recovery released the proxy route")
			}
			if remaining, _ := leaseStore.List(ctx); len(remaining) != 0 {
				t.Fatalf("checkpoint leases remain: %#v", remaining)
			}
		})
	}
}

// unpublishProbeCapture records the route table's view while the capture has
// the VM frozen, before any failure path can restore the route.
type unpublishProbeCapture struct {
	*recordingCheckpointCapture
	id              string
	routes          *fakeRouteTable
	routeVisible    bool
	slotUnpublished bool
}

func (c *unpublishProbeCapture) Capture(ctx context.Context, req RuntimeCaptureRequest) (CapturedBootComponents, error) {
	_, c.routeVisible = c.routes.routeIP(c.id)
	c.slotUnpublished = c.routes.unpublished(c.id)
	return c.recordingCheckpointCapture.Capture(ctx, req)
}

// TestPauseWithdrawsRouteUntilFailureRecovery pins the data-plane view of a
// pause in flight: the route withdraws before the capture freezes the VM, and
// a failure that hands the VM back running republishes the original IP.
func TestPauseWithdrawsRouteUntilFailureRecovery(t *testing.T) {
	m, store, _, capture, routes, _, original := pauseFixture(t, readyBoot())
	// Publish the route the create kernel would have left behind.
	if err := routes.Publish("sandbox-a", original.IP, original.MaskRequestHost); err != nil {
		t.Fatal(err)
	}
	probe := &unpublishProbeCapture{id: "sandbox-a", routes: routes, recordingCheckpointCapture: capture}
	m.checkpointCapture = probe
	// The publish fails after the capture handed the VM over suspended; the
	// recovery resume hands it back running behind the untouched READY record.
	probe.result.MemRootPath = filepath.Join(t.TempDir(), "missing")
	status := &atomic.Int32{}
	status.Store(http.StatusNoContent)
	resumeEntry, resumeHits := vmmResumeEntry(t, status)
	m.sandboxes.Store("sandbox-a", resumeEntry)
	ctx := context.Background()

	err := m.Pause(ctx, "sandbox-a", time.Time{})
	if !errors.Is(err, ErrCheckpointCaptured) {
		t.Fatalf("Pause() error = %v, want the post-capture failure", err)
	}
	if resumeHits.Load() != 1 {
		t.Fatalf("resume requests = %d, want one recovery resume", resumeHits.Load())
	}
	if probe.routeVisible {
		t.Fatal("capture froze the VM with the route still published")
	}
	if !probe.slotUnpublished {
		t.Fatal("withdrawn slot not reported as unpublished during the capture")
	}
	ip, ok := routes.routeIP("sandbox-a")
	if !ok || ip != original.IP {
		t.Fatalf("recovered route = %q, %v, want the original IP %q", ip, ok, original.IP)
	}
	routes.mu.Lock()
	mask := routes.masks["sandbox-a"]
	routes.mu.Unlock()
	if mask != original.MaskRequestHost {
		t.Fatalf("recovered mask = %q, want %q", mask, original.MaskRequestHost)
	}
	rec, getErr := store.Get(ctx, "sandbox-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if rec.State != StateReady || rec.IP != original.IP {
		t.Fatalf("recovered record = %#v", rec)
	}
}

// TestPauseCommitFailureDegradesWhenResumeFails covers the fallback: a VMM
// stop failure whose recovery resume fails too — the control plane died with
// the process — makes undoSuspendedCheckpoint retry and degrade both sides to
// UNKNOWN while the record keeps the pristine running view.
func TestPauseCommitFailureDegradesWhenResumeFails(t *testing.T) {
	m, store, _, _, routes, capacity, original := pauseFixture(t, readyBoot())
	// The delete endpoint fails so Stop reports the failure while the process
	// still dies; the api socket went away with it, so both resume attempts
	// dial a dead control plane.
	entry, resumeHits, deleteHits := vmmStopFailureEntry(t, false, http.StatusInternalServerError, http.StatusNoContent)
	m.sandboxes.Store("sandbox-a", entry)
	ctx := context.Background()

	err := m.Pause(ctx, "sandbox-a", time.Time{})
	if !strings.Contains(err.Error(), "failed to stop VMM") {
		t.Fatalf("Pause() error = %v, want the stop failure", err)
	}
	if !strings.Contains(err.Error(), "failed to resume VM") {
		t.Fatalf("Pause() error = %v, want the failed recovery resume", err)
	}
	if resumeHits.Load() != 0 || deleteHits.Load() != 1 {
		t.Fatalf("resume/delete requests = %d/%d, want 0/1", resumeHits.Load(), deleteHits.Load())
	}
	rec, getErr := store.Get(ctx, "sandbox-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if rec.State != StateUnknown || rec.LastError == "" {
		t.Fatalf("degraded record = %#v", rec)
	}
	if rec.ExpiresAt != original.ExpiresAt || rec.IP != original.IP || rec.VMMPID != original.VMMPID ||
		len(rec.RuntimeSnapshots) != len(original.RuntimeSnapshots) {
		t.Fatalf("degraded record lost the running view: %#v", rec)
	}
	if value, ok := m.sandboxes.Load("sandbox-a"); !ok || value.(*sandboxEntry).state != StateUnknown {
		t.Fatal("degraded runtime entry missing or not unknown")
	}
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("degradation released the capacity reservation")
	}
	if routes.releaseCount("sandbox-a") != 0 {
		t.Fatal("degradation released the proxy route")
	}
}

// TestPausePausedWriteFailureKeepsStoppedRuntime pins the failure after the
// VMM died: a failed PAUSED write must not resume the stopped runtime. The
// record degrades to UNKNOWN keeping its PID and running view for the delete
// retry.
func TestPausePausedWriteFailureKeepsStoppedRuntime(t *testing.T) {
	m, store, _, _, routes, capacity, original := pauseFixture(t, readyBoot())
	status := &atomic.Int32{}
	status.Store(http.StatusNoContent)
	entry, resumeHits := vmmResumeEntry(t, status)
	m.sandboxes.Store("sandbox-a", entry)
	store.updateHook = func(rec Record) error {
		if rec.State == StatePaused {
			return errPausedWrite
		}
		return nil
	}
	ctx := context.Background()

	err := m.Pause(ctx, "sandbox-a", time.Time{})
	if !errors.Is(err, errPausedWrite) {
		t.Fatalf("Pause() error = %v, want the paused-write failure", err)
	}
	if resumeHits.Load() != 0 {
		t.Fatalf("resume requests = %d, want none for a stopped runtime", resumeHits.Load())
	}
	rec, getErr := store.Get(ctx, "sandbox-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if rec.State != StateUnknown || rec.LastError == "" {
		t.Fatalf("retained record = %#v", rec)
	}
	if rec.IP != original.IP || rec.VMMPID != original.VMMPID || rec.ExpiresAt != original.ExpiresAt ||
		len(rec.RuntimeSnapshots) != len(original.RuntimeSnapshots) ||
		rec.CheckpointHeadTemplateID != original.CheckpointHeadTemplateID {
		t.Fatalf("retained record lost the running view: %#v", rec)
	}
	value, entryHeld := m.sandboxes.Load("sandbox-a")
	if !entryHeld || value.(*sandboxEntry).state != StateUnknown {
		t.Fatal("retained entry missing or not unknown")
	}
	if value.(*sandboxEntry).sbx.process != nil {
		t.Fatal("stop phase did not complete before the failed write")
	}
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("paused-write failure released the capacity reservation")
	}
	if routes.releaseCount("sandbox-a") != 0 {
		t.Fatal("paused-write failure released the proxy route")
	}

	// An explicit delete finishes the retained record.
	store.updateHook = nil
	if err := m.Delete(ctx, "sandbox-a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, "sandbox-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retained record = %v", err)
	}
	if _, ok := capacity.holding("sandbox-a"); ok || capacity.releaseCount("sandbox-a") != 1 {
		t.Fatal("explicit delete kept or leaked the capacity reservation")
	}
}

// TestPauseSocketRemovalFailureKeepsStoppedRuntime pins the stop-phase failure
// after the VMM died: an unremovable socket keeps the process handle and the
// record's running view so a delete retries the release through them.
func TestPauseSocketRemovalFailureKeepsStoppedRuntime(t *testing.T) {
	m, store, _, _, routes, capacity, original := pauseFixture(t, readyBoot())
	socket := filepath.Join(t.TempDir(), "vmm.sock")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	process := &vmm.Process{VmmSocketPath: socket, VsockSocketPath: "invalid\x00socket"}
	m.sandboxes.Store("sandbox-a", &sandboxEntry{state: StateReady, sbx: &Sandbox{process: process}})
	ctx := context.Background()

	err := m.Pause(ctx, "sandbox-a", time.Time{})
	if err == nil || !strings.Contains(err.Error(), "remove sandbox socket") {
		t.Fatalf("Pause() error = %v, want the socket removal failure", err)
	}
	rec, getErr := store.Get(ctx, "sandbox-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if rec.State != StateUnknown || rec.LastError == "" {
		t.Fatalf("retained record = %#v", rec)
	}
	if rec.IP != original.IP || rec.VMMPID != original.VMMPID ||
		rec.CheckpointHeadTemplateID != original.CheckpointHeadTemplateID {
		t.Fatalf("retained record lost the running view: %#v", rec)
	}
	value, entryHeld := m.sandboxes.Load("sandbox-a")
	if !entryHeld || value.(*sandboxEntry).state != StateUnknown {
		t.Fatal("retained entry missing or not unknown")
	}
	if value.(*sandboxEntry).sbx.process != process {
		t.Fatal("stopped runtime lost its process before the release was confirmed")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("the vmm socket was not removed before the failure")
	}
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("socket removal failure released the capacity reservation")
	}
	if routes.releaseCount("sandbox-a") != 0 {
		t.Fatal("socket removal failure released the proxy route")
	}

	// An explicit delete retries the socket removal and finishes the record.
	process.VsockSocketPath = filepath.Join(t.TempDir(), "vsock.sock")
	if err := os.WriteFile(process.VsockSocketPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "sandbox-a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, "sandbox-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retained record = %v", err)
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("entry remains after the explicit delete")
	}
	if _, ok := capacity.holding("sandbox-a"); ok || capacity.releaseCount("sandbox-a") != 1 {
		t.Fatal("explicit delete kept or leaked the capacity reservation")
	}
}

func TestResumePausedRestoresSandboxUnderSameID(t *testing.T) {
	m, store, _, _, routes, capacity, _ := pauseFixture(t, readyBoot())
	ctx := context.Background()
	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatal(err)
	}
	paused, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}

	result, err := m.ResumePaused(ctx, "sandbox-a", ResumePausedOptions{
		Timeout: 5 * time.Minute, VMMName: "cloud-hypervisor", VCPUMax: 4,
	})
	if err != nil {
		t.Fatalf("ResumePaused() error = %v", err)
	}
	if result.SandboxID != "sandbox-a" || result.EnvdVersion != "1.2.3" {
		t.Fatalf("resume result = %#v", result)
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateReady || rec.LastError != "" {
		t.Fatalf("resumed record = %#v", rec)
	}
	if rec.CheckpointHeadTemplateID != paused.CheckpointHeadTemplateID {
		t.Fatalf("resume moved the checkpoint head: %#v", rec)
	}
	if rec.StartedAt <= paused.StartedAt {
		t.Fatalf("resume kept the old run start: %#v", rec)
	}
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 4*time.Minute || remaining > 5*time.Minute {
		t.Fatalf("resume timeout = %v", remaining)
	}
	if rec.TimeoutAction != paused.TimeoutAction || rec.MaskRequestHost != paused.MaskRequestHost ||
		rec.Metadata["team"] != "core" || rec.SourceTemplateID != paused.SourceTemplateID {
		t.Fatalf("resume dropped pause-time fields: %#v", rec)
	}
	if reservation, ok := capacity.holding("sandbox-a"); !ok || reservation != (capacityReservation{cpus: 2, memoryMB: 512}) {
		t.Fatalf("capacity reservation = %+v, %v", reservation, ok)
	}
	if len(routes.inserted) != 2 || routes.releaseCount("sandbox-a") != 1 {
		t.Fatalf("route inserts = %#v, releases = %d", routes.inserted, routes.releaseCount("sandbox-a"))
	}
	routes.mu.Lock()
	mask := routes.masks["sandbox-a"]
	routes.mu.Unlock()
	if mask != paused.MaskRequestHost {
		t.Fatalf("published mask = %q", mask)
	}
	value, ok := m.sandboxes.Load("sandbox-a")
	if !ok || value.(*sandboxEntry).state != StateReady {
		t.Fatal("resumed runtime entry missing or not ready")
	}
}

// TestResumePausedConcurrentJoiners covers the connect race: callers that all
// read PAUSED serialize on the lifecycle lock, the winner restores, and the
// losers succeed as joined requests — exactly one restore runs and the
// winner's deadline stands.
func TestResumePausedConcurrentJoiners(t *testing.T) {
	m, store, _, _, routes, capacity, _ := pauseFixture(t, readyBoot())
	ctx := context.Background()
	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatal(err)
	}

	const joiners = 3
	errs := make(chan error, joiners)
	for range joiners {
		go func() {
			_, err := m.ResumePaused(ctx, "sandbox-a", ResumePausedOptions{Timeout: 2 * time.Minute, VMMName: "cloud-hypervisor"})
			errs <- err
		}()
	}
	for range joiners {
		if err := <-errs; err != nil {
			t.Fatalf("joined resume = %v", err)
		}
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateReady || rec.LastError != "" {
		t.Fatalf("joined record = %#v", rec)
	}
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < time.Minute || remaining > 2*time.Minute {
		t.Fatalf("winner deadline = %v", remaining)
	}
	if len(routes.inserted) != 2 || routes.releaseCount("sandbox-a") != 1 {
		t.Fatalf("route inserts = %#v, releases = %d", routes.inserted, routes.releaseCount("sandbox-a"))
	}
	if reservation, ok := capacity.holding("sandbox-a"); !ok || reservation != (capacityReservation{cpus: 2, memoryMB: 512}) {
		t.Fatalf("capacity reservation = %+v, %v", reservation, ok)
	}
	value, ok := m.sandboxes.Load("sandbox-a")
	if !ok || value.(*sandboxEntry).state != StateReady {
		t.Fatal("joined runtime entry missing or not ready")
	}
}

func TestResumePausedFailureWritesBackPausedRecord(t *testing.T) {
	m, store, _, _, routes, capacity, _ := pauseFixture(t, readyBoot())
	ctx := context.Background()
	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatal(err)
	}
	paused, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	launchErr := errors.New("launch failed")
	m.launch = func(context.Context, CreateRequest, VMStartSpec, createRuntimeIDs, bool) (*Sandbox, error) {
		return nil, launchErr
	}

	_, err = m.ResumePaused(ctx, "sandbox-a", ResumePausedOptions{Timeout: time.Minute, VMMName: "cloud-hypervisor"})
	if !errors.Is(err, launchErr) {
		t.Fatalf("ResumePaused() error = %v", err)
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StatePaused || rec.LastError != "" || rec.IP != "" || rec.VMMPID != 0 || rec.ExpiresAt != 0 {
		t.Fatalf("failed resume record = %#v", rec)
	}
	if rec.CheckpointHeadTemplateID != paused.CheckpointHeadTemplateID || rec.StartedAt != rec.HeadStartedAt {
		t.Fatalf("failed resume lost the paused view: %#v", rec)
	}
	if _, ok := capacity.holding("sandbox-a"); ok || capacity.releaseCount("sandbox-a") != 2 {
		t.Fatal("failed resume kept or leaked the capacity reservation")
	}
	if routes.releaseCount("sandbox-a") != 2 {
		t.Fatalf("route releases = %d", routes.releaseCount("sandbox-a"))
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("failed resume kept the runtime entry blocking a retry")
	}
}

// TestResumePausedStrandedUnknownBootsAgain pins the terminal-failure
// recovery: a resume whose confirmed release lost the PAUSED write strands
// the record as UNKNOWN with its lineage, and the next ResumePaused boots it
// from that stranded state under the same ID.
func TestResumePausedStrandedUnknownBootsAgain(t *testing.T) {
	m, store, _, _, routes, capacity, _ := pauseFixture(t, readyBoot())
	ctx := context.Background()
	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatal(err)
	}
	paused, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	workingLaunch := m.launch
	m.launch = func(context.Context, CreateRequest, VMStartSpec, createRuntimeIDs, bool) (*Sandbox, error) {
		return nil, errors.New("launch failed")
	}
	writeErr := errors.New("paused write failure")
	store.updateHook = func(r Record) error {
		if r.State == StatePaused {
			return writeErr
		}
		return nil
	}

	if _, err := m.ResumePaused(ctx, "sandbox-a", ResumePausedOptions{Timeout: time.Minute, VMMName: "cloud-hypervisor"}); err == nil || !strings.Contains(err.Error(), writeErr.Error()) {
		t.Fatalf("ResumePaused() error = %v", err)
	}
	stranded, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if stranded.State != StateUnknown || stranded.LastError != writeErr.Error() {
		t.Fatalf("stranded record = %#v", stranded)
	}
	if stranded.CheckpointHeadTemplateID != paused.CheckpointHeadTemplateID {
		t.Fatalf("stranded record lost its lineage: %#v", stranded)
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("stranded record kept the runtime entry")
	}
	if _, ok := capacity.holding("sandbox-a"); ok {
		t.Fatal("stranded record kept the capacity reservation")
	}

	m.launch, store.updateHook = workingLaunch, nil
	result, err := m.ResumePaused(ctx, "sandbox-a", ResumePausedOptions{Timeout: 5 * time.Minute, VMMName: "cloud-hypervisor"})
	if err != nil {
		t.Fatalf("ResumePaused() retry error = %v", err)
	}
	if result.SandboxID != "sandbox-a" || result.EnvdVersion != "1.2.3" {
		t.Fatalf("retry result = %#v", result)
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateReady || rec.LastError != "" {
		t.Fatalf("resumed record = %#v", rec)
	}
	if rec.CheckpointHeadTemplateID != paused.CheckpointHeadTemplateID {
		t.Fatalf("retry moved the checkpoint head: %#v", rec)
	}
	if reservation, ok := capacity.holding("sandbox-a"); !ok || reservation != (capacityReservation{cpus: 2, memoryMB: 512}) {
		t.Fatalf("capacity reservation = %+v, %v", reservation, ok)
	}
	if routes.releaseCount("sandbox-a") != 2 {
		t.Fatalf("route releases = %d", routes.releaseCount("sandbox-a"))
	}
	if value, ok := m.sandboxes.Load("sandbox-a"); !ok || value.(*sandboxEntry).state != StateReady {
		t.Fatal("resumed runtime entry missing or not ready")
	}
}

// TestResumePausedFinishesStrandedPauseCleanup pins the stranded-pause
// recovery: a pause whose cleanup failed and whose failure write failed
// leaves a PAUSED record holding the spent Unknown entry, the capacity
// reservation and the create lease. A connect whose cleanup retry still
// fails answers that error with the strand untouched; the next connect
// finishes the release — cleanup, lease, entry, reservation — and resumes
// through the normal path instead of waiting for a node restart.
func TestResumePausedFinishesStrandedPauseCleanup(t *testing.T) {
	b := readyBoot()
	b.releaseErr = errors.New("boot release failed")
	m, store, leaseStore, _, routes, capacity, _ := pauseFixture(t, b)
	ctx := context.Background()
	// The pause's terminal PAUSED write succeeds; the failure write of its
	// failed cleanup (UNKNOWN) fails, so the record stays PAUSED on disk.
	writeErr := errors.New("persist failure")
	store.updateHook = func(r Record) error {
		if r.State == StateUnknown {
			return writeErr
		}
		return nil
	}
	leaseID := "sandbox-create-sandbox-a"
	leaseStore.items[leaseID] = leases.Lease{ID: leaseID}

	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err == nil {
		t.Fatal("pause with failing cleanup and write-back succeeded")
	}
	strand, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if strand.State != StatePaused || strand.CheckpointHeadTemplateID == "" {
		t.Fatalf("stranded record = %#v", strand)
	}
	if value, ok := m.sandboxes.Load("sandbox-a"); !ok || value.(*sandboxEntry).state != StateUnknown {
		t.Fatalf("stranded entry = %#v, %v", value, ok)
	}
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("stranded pause lost the capacity reservation")
	}

	// A connect while the cleanup still fails answers the cleanup error and
	// leaves the strand for the next attempt.
	if _, err := m.ResumePaused(ctx, "sandbox-a", ResumePausedOptions{Timeout: time.Minute, VMMName: "cloud-hypervisor"}); err == nil {
		t.Fatal("resume with a still-failing stranded cleanup succeeded")
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); !ok {
		t.Fatal("failed stranded cleanup dropped the entry")
	}
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("failed stranded cleanup released the reservation")
	}
	if _, held := leaseStore.items[leaseID]; !held {
		t.Fatal("failed stranded cleanup released the create lease")
	}
	if rec, err := store.Get(ctx, "sandbox-a"); err != nil || rec.State != StatePaused {
		t.Fatalf("failed stranded cleanup moved the record: %#v (%v)", rec, err)
	}

	// The next connect finishes the release and resumes.
	b.releaseErr = nil
	store.updateHook = nil
	result, err := m.ResumePaused(ctx, "sandbox-a", ResumePausedOptions{Timeout: 5 * time.Minute, VMMName: "cloud-hypervisor"})
	if err != nil {
		t.Fatalf("ResumePaused() retry error = %v", err)
	}
	if result.SandboxID != "sandbox-a" || result.EnvdVersion != "1.2.3" {
		t.Fatalf("retry result = %#v", result)
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateReady || rec.LastError != "" || rec.CheckpointHeadTemplateID != strand.CheckpointHeadTemplateID {
		t.Fatalf("resumed record = %#v", rec)
	}
	// The strand's reservation released exactly once, then the resume
	// reserved it again — without the tail the Reserve would fail
	// ErrAlreadyExists and the strand would never recover in-process.
	if reservation, ok := capacity.holding("sandbox-a"); !ok || reservation != (capacityReservation{cpus: 2, memoryMB: 512}) {
		t.Fatalf("capacity reservation = %+v, %v", reservation, ok)
	}
	if got := capacity.releaseCount("sandbox-a"); got != 1 {
		t.Fatalf("capacity releases = %d", got)
	}
	if _, held := leaseStore.items[leaseID]; held {
		t.Fatal("stranded create lease remains after the resume")
	}
	// Three route releases: the pause's failed cleanup and both cleanup
	// retries each release the idempotent slot before their later steps.
	if got := routes.releaseCount("sandbox-a"); got != 3 {
		t.Fatalf("route releases = %d", got)
	}
	if value, ok := m.sandboxes.Load("sandbox-a"); !ok || value.(*sandboxEntry).state != StateReady {
		t.Fatal("resumed runtime entry missing or not ready")
	}
}

func TestResumePausedGuards(t *testing.T) {
	m, store, _, _, _, _, _ := pauseFixture(t, readyBoot())
	m.sandboxes.Delete("sandbox-a")
	capture := &recordingCheckpointCapture{}
	m.checkpointCapture = capture
	ctx := context.Background()
	base := testCreateRequest()

	if _, err := m.ResumePaused(ctx, "", ResumePausedOptions{Timeout: time.Minute}); err == nil {
		t.Fatal("empty id accepted")
	}
	if _, err := m.ResumePaused(ctx, "sandbox-a", ResumePausedOptions{}); err == nil {
		t.Fatal("zero timeout accepted")
	}
	m.Routes = nil
	if _, err := m.ResumePaused(ctx, "sandbox-a", ResumePausedOptions{Timeout: time.Minute}); err == nil {
		t.Fatal("missing dependencies accepted")
	}
	m.Routes = newFakeRouteTable()
	for _, tc := range []struct {
		name   string
		record Record
	}{
		{"native", Record{ID: "native", State: StatePaused, E2B: false, CheckpointHeadTemplateID: base.TemplateID}},
		{"creating", Record{ID: "creating", State: StateCreating, E2B: true, SourceTemplateID: base.TemplateID}},
		{"paused without head", Record{ID: "headless", State: StatePaused, E2B: true, CheckpointHeadTemplateID: base.TemplateID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Create(ctx, tc.record); err != nil {
				t.Fatal(err)
			}
			if tc.name == "paused without head" {
				// The store refuses to persist a headless record; strip the
				// head directly to reach the defensive resume guard.
				store.mu.Lock()
				rec := store.records[tc.record.ID]
				rec.CheckpointHeadTemplateID = ""
				store.records[tc.record.ID] = rec
				store.mu.Unlock()
			}
			if _, err := m.ResumePaused(ctx, tc.record.ID, ResumePausedOptions{Timeout: time.Minute}); err == nil {
				t.Fatal("invalid resume accepted")
			}
		})
	}
	// A record already READY under the lock is a concurrent resume that won
	// the race: the call succeeds as the joined request, without a restore
	// and without touching the winner's deadline.
	readyRec := Record{ID: "ready", State: StateReady, E2B: true, CheckpointHeadTemplateID: base.TemplateID,
		StartedAt: time.Now().UnixNano(), ExpiresAt: time.Now().Add(time.Hour).UnixNano()}
	if _, err := store.Create(ctx, readyRec); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ResumePaused(ctx, "ready", ResumePausedOptions{Timeout: time.Minute}); err != nil {
		t.Fatalf("joined resume = %v", err)
	}
	joined, err := store.Get(ctx, "ready")
	if err != nil {
		t.Fatal(err)
	}
	if joined.State != StateReady || joined.ExpiresAt != readyRec.ExpiresAt {
		t.Fatalf("joined resume mutated the record: %#v", joined)
	}

	// A leftover runtime entry blocks the in-place resume — for every state
	// but a stranded pause's, whose Unknown entry the resume finishes and
	// recovers instead. The store refuses to persist CREATING records, so
	// the occupied record is seeded as PAUSED and degraded directly.
	if _, err := store.Create(ctx, Record{ID: "occupied", State: StatePaused, E2B: true, CheckpointHeadTemplateID: base.TemplateID}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	occupied := store.records["occupied"]
	occupied.State = StateCreating
	store.records["occupied"] = occupied
	store.mu.Unlock()
	m.sandboxes.Store("occupied", &sandboxEntry{state: StateUnknown})
	if _, err := m.ResumePaused(ctx, "occupied", ResumePausedOptions{Timeout: time.Minute}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("occupied resume = %v", err)
	}
	if len(capture.requests) != 0 {
		t.Fatalf("rejected resumes captured: %#v", capture.requests)
	}
}

// TestEnsureRunning covers the wake dispatch: a READY sandbox is a no-op, a
// paused sandbox without autoResume is refused, and an armed sandbox is
// restored under the same lock — concurrent wakers serialize, so exactly one
// restore runs and the loser returns on the READY record.
func TestEnsureRunning(t *testing.T) {
	m, store, _, _, routes, capacity, _ := pauseFixture(t, readyBoot())
	ctx := context.Background()
	base := testCreateRequest()

	if err := m.EnsureRunning(ctx, "", ResumePausedOptions{Timeout: time.Minute}); err == nil {
		t.Fatal("empty id accepted")
	}
	if err := m.EnsureRunning(ctx, "sandbox-a", ResumePausedOptions{}); err == nil {
		t.Fatal("zero timeout accepted")
	}
	if err := m.EnsureRunning(ctx, "missing", ResumePausedOptions{Timeout: time.Minute}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown sandbox = %v", err)
	}
	if _, err := store.Create(ctx, Record{
		ID: "native", State: StateReady, E2B: false, CheckpointHeadTemplateID: base.TemplateID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureRunning(ctx, "native", ResumePausedOptions{Timeout: time.Minute}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("native sandbox = %v", err)
	}

	// A running sandbox is a no-op that never touches its deadline.
	if err := m.EnsureRunning(ctx, "sandbox-a", ResumePausedOptions{Timeout: time.Minute}); err != nil {
		t.Fatalf("ready no-op = %v", err)
	}
	ready, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if ready.ExpiresAt != 999 || len(routes.inserted) != 1 || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatalf("ready no-op touched the sandbox: %#v", ready)
	}

	if err := m.Pause(ctx, "sandbox-a", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureRunning(ctx, "sandbox-a", ResumePausedOptions{Timeout: time.Minute}); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("paused without autoResume = %v", err)
	}
	paused, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != StatePaused {
		t.Fatalf("refused wake mutated the record: %#v", paused)
	}

	// Armed with autoResume: the wake restores under the same ID with the
	// granted TTL, and concurrent wakers run exactly one restore.
	paused.AutoResume = true
	if _, err := store.Update(ctx, paused); err != nil {
		t.Fatal(err)
	}
	const wakers = 2
	errs := make(chan error, wakers)
	for range wakers {
		go func() {
			errs <- m.EnsureRunning(ctx, "sandbox-a", ResumePausedOptions{Timeout: 2 * time.Minute, VMMName: "cloud-hypervisor"})
		}()
	}
	for range wakers {
		if err := <-errs; err != nil {
			t.Fatalf("armed wake = %v", err)
		}
	}
	woken, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if woken.State != StateReady || !woken.AutoResume {
		t.Fatalf("woken record = %#v", woken)
	}
	if remaining := time.Until(time.Unix(0, woken.ExpiresAt)); remaining < time.Minute || remaining > 2*time.Minute {
		t.Fatalf("wake timeout = %v", remaining)
	}
	// One restore in total: the fixture's create-time insert, then the wake's.
	if len(routes.inserted) != 2 || routes.releaseCount("sandbox-a") != 1 {
		t.Fatalf("route inserts = %#v, releases = %d", routes.inserted, routes.releaseCount("sandbox-a"))
	}
	if reservation, ok := capacity.holding("sandbox-a"); !ok || reservation != (capacityReservation{cpus: 2, memoryMB: 512}) {
		t.Fatalf("capacity reservation = %+v, %v", reservation, ok)
	}
	value, ok := m.sandboxes.Load("sandbox-a")
	if !ok || value.(*sandboxEntry).state != StateReady {
		t.Fatal("woken runtime entry missing or not ready")
	}

	// A stranded lineage — the residue of a terminal write that failed after
	// the release was confirmed — follows the same autoResume gate: without
	// the arming only a manual connect revives it, an armed one restores.
	stranded := Record{
		ID: "stranded", State: StateUnknown, E2B: true, LastError: "paused write failure",
		SourceTemplateID: base.TemplateID, CheckpointHeadTemplateID: digest.FromString("stranded-head").String(),
		VCPUNum: base.VCPUNum, RamMB: base.RAMMB, HeadStartedAt: 100, HeadCreatedAt: 200,
	}
	if _, err := store.Create(ctx, stranded); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureRunning(ctx, "stranded", ResumePausedOptions{Timeout: time.Minute}); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("stranded without autoResume = %v", err)
	}
	got, err := store.Get(ctx, "stranded")
	if err != nil || got.State != StateUnknown {
		t.Fatalf("refused stranded wake mutated the record: %#v (%v)", got, err)
	}
	stranded.AutoResume = true
	if _, err := store.Update(ctx, stranded); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureRunning(ctx, "stranded", ResumePausedOptions{Timeout: 2 * time.Minute, VMMName: "cloud-hypervisor"}); err != nil {
		t.Fatalf("armed stranded wake = %v", err)
	}
	revived, err := store.Get(ctx, "stranded")
	if err != nil || revived.State != StateReady || !revived.AutoResume {
		t.Fatalf("revived record = %#v (%v)", revived, err)
	}
	if remaining := time.Until(time.Unix(0, revived.ExpiresAt)); remaining < time.Minute || remaining > 2*time.Minute {
		t.Fatalf("stranded wake timeout = %v", remaining)
	}
}

func TestApplyTTL(t *testing.T) {
	m, store, _, _, _, _, _ := pauseFixture(t, readyBoot())
	m.sandboxes.Delete("sandbox-a")
	ctx := context.Background()
	base := testCreateRequest()
	seed := func(id string, startedAt, expiresAt int64) Record {
		rec, err := store.Create(ctx, Record{
			ID: id, State: StateReady, E2B: true, CheckpointHeadTemplateID: base.TemplateID,
			StartedAt: startedAt, ExpiresAt: expiresAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}

	// set_timeout semantics: reset, may shorten, zero or negative expires now.
	rec := seed("reset", time.Now().UnixNano(), time.Now().Add(time.Hour).UnixNano())
	updated, err := m.ApplyTTL(ctx, "reset", TTLRequest{TTL: time.Minute, MaxLifetime: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(time.Unix(0, updated.ExpiresAt)); remaining > time.Minute || remaining < 50*time.Second {
		t.Fatalf("reset deadline = %v", remaining)
	}
	updated, err = m.ApplyTTL(ctx, "reset", TTLRequest{TTL: -time.Second, MaxLifetime: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ExpiresAt > time.Now().UnixNano() {
		t.Fatalf("negative TTL did not expire now: %v", time.Until(time.Unix(0, updated.ExpiresAt)))
	}

	// extend-only: a no-op when the deadline would not move forward.
	rec = seed("extend", time.Now().UnixNano(), time.Now().Add(time.Hour).UnixNano())
	updated, err = m.ApplyTTL(ctx, "extend", TTLRequest{TTL: time.Minute, ExtendOnly: true, MaxLifetime: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ExpiresAt != rec.ExpiresAt {
		t.Fatalf("extend-only shortened the deadline: %v", time.Until(time.Unix(0, updated.ExpiresAt)))
	}
	updated, err = m.ApplyTTL(ctx, "extend", TTLRequest{ExtendOnly: true, MaxLifetime: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ExpiresAt != rec.ExpiresAt {
		t.Fatal("extend-only without a TTL changed the deadline")
	}
	// A shorter run is still clamped by the total lifetime cap.
	seed("aged", time.Now().Add(-23*time.Hour).UnixNano(), 0)
	updated, err = m.ApplyTTL(ctx, "aged", TTLRequest{TTL: 48 * time.Hour, MaxLifetime: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(time.Unix(0, updated.ExpiresAt)); remaining > time.Hour {
		t.Fatalf("lifetime cap ignored: %v", remaining)
	}
	// A run past the total lifetime is rejected outright.
	seed("expired-life", time.Now().Add(-25*time.Hour).UnixNano(), 0)
	_, err = m.ApplyTTL(ctx, "expired-life", TTLRequest{TTL: time.Minute, MaxLifetime: 24 * time.Hour})
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.PublicMessage() != "Max instance length exceeded" {
		t.Fatalf("over-lifetime TTL = %v", err)
	}

	// Guards: unknown, native, non-ready.
	if _, err := m.ApplyTTL(ctx, "missing", TTLRequest{}); err == nil {
		t.Fatal("unknown sandbox accepted")
	}
	if _, err := store.Create(ctx, Record{ID: "native", State: StateReady, CheckpointHeadTemplateID: base.TemplateID}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyTTL(ctx, "native", TTLRequest{}); err == nil {
		t.Fatal("native sandbox accepted")
	}
	if _, err := store.Create(ctx, Record{ID: "paused", State: StatePaused, E2B: true, CheckpointHeadTemplateID: base.TemplateID}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyTTL(ctx, "paused", TTLRequest{}); err == nil {
		t.Fatal("paused sandbox accepted")
	}
}

// TestExpireDeletesWhateverTheAction pins the expiry verdict: the dispatch
// reaching Expire — the kill action or the dead-runtime fallback of a failed
// pause — always deletes, whatever the timeout action and lineage.
func TestExpireDeletesWhateverTheAction(t *testing.T) {
	ctx := context.Background()
	base := testCreateRequest()

	for _, tc := range []struct {
		name         string
		record       Record
		withEntry    bool
		wantReleases int
	}{
		{
			name: "kill action deletes a recoverable lineage",
			record: Record{
				ID: "lineage", State: StateReady, E2B: true, SourceTemplateID: base.TemplateID,
				TimeoutAction:            runtimeapi.TimeoutActionDelete,
				CheckpointHeadTemplateID: digest.FromString("head").String(),
				StartedAt:                100, HeadStartedAt: 100, HeadCreatedAt: 200, ExpiresAt: 1, IP: "10.0.0.2",
			},
			withEntry:    true,
			wantReleases: 1,
		},
		{
			name: "pause action verdict also deletes",
			record: Record{
				ID: "lineage-pause", State: StateReady, E2B: true, SourceTemplateID: base.TemplateID,
				TimeoutAction:            runtimeapi.TimeoutActionPause,
				CheckpointHeadTemplateID: digest.FromString("head").String(),
				StartedAt:                100, HeadStartedAt: 100, HeadCreatedAt: 200, ExpiresAt: 1, IP: "10.0.0.2",
			},
			withEntry:    true,
			wantReleases: 1,
		},
		{
			name: "fresh lineage deletes",
			record: Record{
				ID: "fresh", State: StateReady, E2B: true, SourceTemplateID: base.TemplateID,
				CheckpointHeadTemplateID: base.TemplateID, ExpiresAt: 1, StartedAt: 100,
			},
			withEntry:    true,
			wantReleases: 1,
		},
		{
			name: "persisted without entry deletes",
			record: Record{
				ID: "entryless", State: StateReady, E2B: true, SourceTemplateID: base.TemplateID,
				CheckpointHeadTemplateID: base.TemplateID, ExpiresAt: 1, StartedAt: 100,
			},
			wantReleases: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, store, _, _, routes, capacity, _ := pauseFixture(t, readyBoot())
			m.sandboxes.Delete("sandbox-a")
			rec, err := store.Create(ctx, tc.record)
			if err != nil {
				t.Fatal(err)
			}
			if tc.withEntry {
				m.sandboxes.Store(rec.ID, &sandboxEntry{state: StateReady, sbx: &Sandbox{}})
			}
			if err := m.Expire(ctx, rec.ID, time.Now()); err != nil {
				t.Fatalf("Expire() error = %v", err)
			}
			if _, err := store.Get(ctx, rec.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("record = %v", err)
			}
			if _, ok := capacity.holding(rec.ID); ok || capacity.releaseCount(rec.ID) != 1 {
				t.Fatal("expire kept or leaked the capacity reservation")
			}
			if routes.releaseCount(rec.ID) != tc.wantReleases {
				t.Fatalf("route releases = %d, want %d", routes.releaseCount(rec.ID), tc.wantReleases)
			}
			if _, ok := m.sandboxes.Load(rec.ID); ok {
				t.Fatal("runtime entry remains after expiry")
			}
		})
	}

	// A refresh that moved the deadline wins over the expiry dispatch.
	m, store, _, capture, _, _, _ := pauseFixture(t, readyBoot())
	m.sandboxes.Delete("sandbox-a")
	capture.err = errors.New("capture must not run")
	m.checkpointCapture = capture
	base2 := testCreateRequest()
	seed := func(id string, expiresAt int64) Record {
		rec, err := store.Create(ctx, Record{
			ID: id, State: StateReady, E2B: true, SourceTemplateID: base2.TemplateID,
			CheckpointHeadTemplateID: base2.TemplateID, ExpiresAt: expiresAt, StartedAt: time.Now().UnixNano(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	live := seed("refreshed", time.Now().Add(time.Minute).UnixNano())
	if err := m.Expire(ctx, "refreshed", time.Now()); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("no-longer-expired = %v", err)
	}
	kept, err := store.Get(ctx, "refreshed")
	if err != nil || kept.State != StateReady || kept.ExpiresAt != live.ExpiresAt {
		t.Fatalf("refreshed record was touched: %#v, %v", kept, err)
	}
	seed("notready", 0)
	rec, _ := store.Get(ctx, "notready")
	rec.State = StatePaused
	if _, err := store.Update(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := m.Expire(ctx, "notready", time.Time{}); !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("paused expire = %v", err)
	}
	if _, err := store.Create(ctx, Record{ID: "native", State: StateReady, CheckpointHeadTemplateID: base2.TemplateID, ExpiresAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.Expire(ctx, "native", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("native expire = %v", err)
	}
	if len(capture.requests) != 0 {
		t.Fatalf("rejected expiries captured: %#v", capture.requests)
	}
}

// TestExpireFailureKeepsDeletionIntentUntilDelete pins the failed-expire
// verdict: every expiry failure persists UNKNOWN with the lineage collapsed,
// whatever the timeout action, so the explicit Delete retry deletes the record
// instead of resurrecting it as PAUSED.
func TestExpireFailureKeepsDeletionIntentUntilDelete(t *testing.T) {
	ctx := context.Background()
	base := testCreateRequest()
	seed := func(t *testing.T, store *memorySandboxStore, id, action string) Record {
		t.Helper()
		rec, err := store.Create(ctx, Record{
			ID: id, State: StateReady, E2B: true, SourceTemplateID: base.TemplateID,
			TimeoutAction:            action,
			CheckpointHeadTemplateID: digest.FromString("head").String(),
			StartedAt:                100, HeadStartedAt: 100, HeadCreatedAt: 200, ExpiresAt: 1, IP: "10.0.0.2",
		})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}

	// A failed cleanup persists the deletion intent for both actions: the
	// lineage collapses so the delete retry does not resurrect it as PAUSED.
	for _, action := range []string{runtimeapi.TimeoutActionDelete, runtimeapi.TimeoutActionPause} {
		b := readyBoot()
		b.releaseErr = errors.New("boot release failed")
		m, store, _, _, _, capacity, _ := pauseFixture(t, b)
		m.sandboxes.Delete("sandbox-a")
		victim := seed(t, store, "expire-"+action, action)
		m.sandboxes.Store(victim.ID, &sandboxEntry{state: StateReady, sbx: &Sandbox{}})
		if err := capacity.Reserve(victim.ID, 1, 512); err != nil {
			t.Fatal(err)
		}
		if err := m.Expire(ctx, victim.ID, time.Now()); err == nil {
			t.Fatal("expire with failing cleanup succeeded")
		}
		got, err := store.Get(ctx, victim.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != StateUnknown || got.CheckpointHeadTemplateID != got.SourceTemplateID || got.LastError == "" {
			t.Fatalf("%s failure record = %#v", action, got)
		}
		if _, ok := capacity.holding(victim.ID); !ok {
			t.Fatal("failed expire dropped the capacity reservation")
		}
		b.releaseErr = nil
		if err := m.Delete(ctx, victim.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Get(ctx, victim.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s delete retry record = %v", action, err)
		}
		if _, ok := capacity.holding(victim.ID); ok {
			t.Fatalf("%s delete retry kept the capacity reservation", action)
		}
	}

	// A failed terminal delete keeps the verdict the same way; the release
	// was confirmed, so the capacity reservation goes with it.
	m3, store3, _, _, _, capacity3, _ := pauseFixture(t, readyBoot())
	m3.sandboxes.Delete("sandbox-a")
	kill3 := seed(t, store3, "kill-delete", runtimeapi.TimeoutActionDelete)
	m3.sandboxes.Store(kill3.ID, &sandboxEntry{state: StateReady, sbx: &Sandbox{}})
	if err := capacity3.Reserve(kill3.ID, 1, 512); err != nil {
		t.Fatal(err)
	}
	store3.deleteHook = func() error { return errors.New("store down") }
	if err := m3.Expire(ctx, kill3.ID, time.Now()); err == nil {
		t.Fatal("expire with failing terminal delete succeeded")
	}
	got3, err := store3.Get(ctx, kill3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got3.State != StateUnknown || got3.CheckpointHeadTemplateID != got3.SourceTemplateID || got3.LastError == "" {
		t.Fatalf("terminal delete failure record = %#v", got3)
	}
	if _, ok := capacity3.holding(kill3.ID); ok {
		t.Fatal("terminal delete failure kept the capacity reservation")
	}
	if _, ok := m3.sandboxes.Load(kill3.ID); ok {
		t.Fatal("terminal delete failure kept the runtime entry")
	}
	store3.deleteHook = nil
	if err := m3.Delete(ctx, kill3.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store3.Get(ctx, kill3.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retried terminal delete record = %v", err)
	}
}
