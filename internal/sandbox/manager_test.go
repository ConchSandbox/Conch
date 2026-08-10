package sandbox

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/cow"
	"github.com/openeuler/Conch/internal/memsnap"
)

func TestDurationOrDefault(t *testing.T) {
	const fallback = 10 * time.Millisecond
	if got := durationOrDefault(0, fallback); got != fallback {
		t.Fatalf("durationOrDefault(0) = %s, want %s", got, fallback)
	}
	if got := durationOrDefault(time.Second, fallback); got != time.Second {
		t.Fatalf("durationOrDefault(1s) = %s, want 1s", got)
	}
}

func TestReserveSandboxEntryDoesNotBlockDifferentSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	defer m.sandboxes.CompareAndDelete(key, entry)
	defer entry.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		otherKey, otherEntry, err := m.reserveSandboxEntry("sandbox-b")
		if err == nil {
			m.sandboxes.CompareAndDelete(otherKey, otherEntry)
			otherEntry.mu.Unlock()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reserveSandboxEntry() for different sandbox error = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("reserveSandboxEntry() for different sandbox blocked behind another sandbox entry")
	}
}

func TestHandleSandboxExitCleansSuspendedSandbox(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{
		boot:         boot,
		cidAllocator: NewCIDAllocator(),
	}
	sbx := &Sandbox{
		cleanup:   NewCleanup(),
		sandboxID: "sandbox-a",
	}
	entry := &sandboxEntry{state: sandboxSuspended, sbx: sbx}
	mapKey := "sandbox-a"
	m.sandboxes.Store(mapKey, entry)

	m.handleSandboxExit(mapKey, entry, "sandbox-a", sbx)

	if _, ok := m.sandboxes.Load(mapKey); ok {
		t.Fatal("suspended sandbox entry remains after VMM exit")
	}
	if len(boot.released) != 1 || boot.released[0].SandboxID != "sandbox-a" {
		t.Fatalf("released boot layouts = %#v", boot.released)
	}
}

func TestCheckpointCapturesRunningAndSuspendedSandbox(t *testing.T) {
	tests := []struct {
		name            string
		initialState    sandboxLifecycleState
		wantPauseBefore bool
	}{
		{name: "running", initialState: sandboxReady, wantPauseBefore: true},
		{name: "suspended", initialState: sandboxSuspended, wantPauseBefore: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := CapturedBootComponents{
				MemRootPath:  "/capture/mem",
				VMMName:      "cloud-hypervisor",
				MemorySizeMB: 512,
			}
			capture := &recordingCheckpointCapture{result: want}
			m, entry, sbx := checkpointTestManager(tt.initialState, capture)

			got, err := m.Checkpoint(CheckpointRequest{SandboxID: "sandbox-a"})
			if err != nil {
				t.Fatalf("Checkpoint() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Checkpoint() result = %#v, want %#v", got, want)
			}
			if entry.state != tt.initialState {
				t.Fatalf("entry state after checkpoint = %s, want %s", entry.state, tt.initialState)
			}
			if len(capture.requests) != 1 {
				t.Fatalf("capture requests = %d, want 1", len(capture.requests))
			}
			if capture.requests[0].Source != sbx {
				t.Fatalf("capture source = %T %p, want sandbox %p", capture.requests[0].Source, capture.requests[0].Source, sbx)
			}
			if capture.requests[0].PauseBefore != tt.wantPauseBefore {
				t.Fatalf("PauseBefore = %v, want %v", capture.requests[0].PauseBefore, tt.wantPauseBefore)
			}
		})
	}
}

func TestCheckpointCaptureErrorRestoresPreviousLifecycleState(t *testing.T) {
	errCapture := errors.New("capture failed")
	tests := []struct {
		name         string
		initialState sandboxLifecycleState
	}{
		{name: "running", initialState: sandboxReady},
		{name: "suspended", initialState: sandboxSuspended},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := &recordingCheckpointCapture{err: errCapture}
			m, entry, _ := checkpointTestManager(tt.initialState, capture)

			_, err := m.Checkpoint(CheckpointRequest{SandboxID: "sandbox-a"})
			if !errors.Is(err, errCapture) {
				t.Fatalf("Checkpoint() error = %v, want errors.Is(capture error)", err)
			}
			if entry.state != tt.initialState {
				t.Fatalf("entry state after capture error = %s, want %s", entry.state, tt.initialState)
			}
		})
	}
}

func TestCheckpointResumeFailureLeavesSandboxSuspended(t *testing.T) {
	errResume := errors.New("resume failed")
	capture := &recordingCheckpointCapture{err: errors.Join(ErrCheckpointResume, errResume)}
	m, entry, _ := checkpointTestManager(sandboxReady, capture)

	_, err := m.Checkpoint(CheckpointRequest{SandboxID: "sandbox-a"})
	if !errors.Is(err, ErrCheckpointResume) || !errors.Is(err, errResume) {
		t.Fatalf("Checkpoint() error = %v, want joined resume failure", err)
	}
	if entry.state != sandboxSuspended {
		t.Fatalf("entry state after resume failure = %s, want %s", entry.state, sandboxSuspended)
	}
}

func TestCheckpointUsesIncrementalCaptureAndRequiresCompletionToClearPoison(t *testing.T) {
	fullCapture := &recordingCheckpointCapture{}
	incrementalCapture := &recordingCheckpointCapture{poisonSource: true, result: CapturedBootComponents{
		MemoryFormat: incrementalMemoryFormat,
		Manifest: &memsnap.Manifest{
			SchemaVersion: memsnap.SchemaVersion,
			MemorySize:    memsnap.DefaultBlockSize,
			BlockSize:     memsnap.DefaultBlockSize,
			Layers:        []string{"layers/0.mem"},
			BuildMap:      []memsnap.BuildRange{{Offset: 0, Length: memsnap.DefaultBlockSize, LayerIndex: 0}},
		},
	}}
	m, entry, sbx := checkpointTestManager(sandboxReady, fullCapture)
	m.incrementalCapture = incrementalCapture
	sbx.memoryMode = "incremental"
	sbx.memoryOrigin = "restored"

	if _, err := m.Checkpoint(CheckpointRequest{SandboxID: "sandbox-a"}); err != nil {
		t.Fatalf("Checkpoint() error = %v", err)
	}
	if len(fullCapture.requests) != 0 || len(incrementalCapture.requests) != 1 {
		t.Fatalf("capture calls: full=%d incremental=%d", len(fullCapture.requests), len(incrementalCapture.requests))
	}
	if sbx.memoryManifest == nil || !sbx.CheckpointPoisoned() {
		t.Fatalf("sandbox memory transaction = manifest %#v poisoned %v", sbx.memoryManifest, sbx.CheckpointPoisoned())
	}
	if _, err := m.Checkpoint(CheckpointRequest{SandboxID: "sandbox-a"}); err == nil || !strings.Contains(err.Error(), "previous incremental") {
		t.Fatalf("second Checkpoint() error = %v", err)
	}
	if err := m.CompleteCheckpoint(LifecycleRequest{SandboxID: "sandbox-a"}); err != nil {
		t.Fatalf("CompleteCheckpoint() error = %v", err)
	}
	if sbx.CheckpointPoisoned() || entry.state != sandboxReady {
		t.Fatalf("completed sandbox state = poisoned %v lifecycle %s", sbx.CheckpointPoisoned(), entry.state)
	}
}

func TestPrepareIncrementalMemoryAttachesAndWaitsWithoutProcessDescriptor(t *testing.T) {
	root := t.TempDir()
	memoryFile, err := os.CreateTemp(t.TempDir(), "memory-")
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingMemoryAttacher{file: memoryFile, response: cow.Response{
		OK: true, Token: "token-1", UFFDSocketPath: "/run/conch/uffd/token-1.sock",
		MemorySize: 1024 * 1024, BlockSize: memsnap.DefaultBlockSize,
	}}
	m := &Manager{memory: client}
	attachment, err := m.prepareIncrementalMemory(context.Background(), "sandbox-a", PreparedBoot{
		Spec: BootSpec{MemorySizeMB: 1},
		Runtime: BootRuntime{
			Resume: true, MemoryFormat: incrementalMemoryFormat, MemorySnapshotRoot: root,
		},
	})
	if err != nil {
		t.Fatalf("prepareIncrementalMemory() error = %v", err)
	}
	if err := attachment.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady() error = %v", err)
	}
	if client.attachRequest.SandboxID != "sandbox-a" || client.attachRequest.MemorySnapshotRoot != root ||
		client.waitToken != "token-1" || client.waitSandboxID != "sandbox-a" {
		t.Fatalf("cow calls = attach %#v wait token %q sandbox %q", client.attachRequest, client.waitToken, client.waitSandboxID)
	}
	if err := attachment.detach(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.detachToken != "token-1" {
		t.Fatalf("detach token = %q", client.detachToken)
	}
}

func TestCreateClosesIncrementalMemoryFileWhenVMMDoesNotTakeOwnership(t *testing.T) {
	memoryFile, err := os.CreateTemp(t.TempDir(), "memory-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = memoryFile.Close() })
	client := &recordingMemoryAttacher{file: memoryFile, response: cow.Response{
		OK: true, Token: "token-1", UFFDSocketPath: "/run/conch/uffd/token-1.sock",
		MemorySize: 1024 * 1024, BlockSize: memsnap.DefaultBlockSize,
	}}
	m := &Manager{
		boot: &recordingBootPreparer{prepared: PreparedBoot{
			Spec: BootSpec{MemorySizeMB: 1},
			Runtime: BootRuntime{
				Resume: true, MemoryFormat: incrementalMemoryFormat, MemorySnapshotRoot: t.TempDir(),
			},
		}},
		memory:         client,
		cidAllocator:   NewCIDAllocator(),
		requestTimeout: time.Second,
	}

	_, err = m.Create(CreateRequest{
		SandboxID:  "sandbox-a",
		AgentToken: "token",
		VMMName:    "stratovirt",
		VCPUNum:    1,
	})
	if err == nil || !strings.Contains(err.Error(), `vmm "stratovirt" is not configured`) {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := memoryFile.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("incremental memory file remains open: %v", err)
	}
	if client.detachToken != "token-1" {
		t.Fatalf("Detach() token = %q", client.detachToken)
	}
}

type recordingMemoryAttacher struct {
	file          *os.File
	response      cow.Response
	attachRequest cow.Request
	waitToken     string
	waitSandboxID string
	detachToken   string
}

func (client *recordingMemoryAttacher) Attach(_ context.Context, request cow.Request) (*os.File, cow.Response, error) {
	client.attachRequest = request
	return client.file, client.response, nil
}
func (client *recordingMemoryAttacher) WaitAttachmentReady(_ context.Context, token, sandboxID string) (cow.Response, error) {
	client.waitToken = token
	client.waitSandboxID = sandboxID
	return cow.Response{OK: true}, nil
}
func (client *recordingMemoryAttacher) Detach(_ context.Context, token string) (cow.Response, error) {
	client.detachToken = token
	return cow.Response{OK: true}, nil
}

type recordingCheckpointCapture struct {
	requests     []RuntimeCaptureRequest
	result       CapturedBootComponents
	err          error
	poisonSource bool
}

func (r *recordingCheckpointCapture) Capture(_ context.Context, req RuntimeCaptureRequest) (CapturedBootComponents, error) {
	r.requests = append(r.requests, req)
	if r.poisonSource {
		if source, ok := req.Source.(interface{ SetCheckpointPoisoned(bool) }); ok {
			source.SetCheckpointPoisoned(true)
		}
	}
	return r.result, r.err
}

func checkpointTestManager(initialState sandboxLifecycleState, capture CheckpointCapture) (*Manager, *sandboxEntry, *Sandbox) {
	m := &Manager{checkpointCapture: capture, requestTimeout: time.Second}
	sbx := &Sandbox{
		sandboxID: "sandbox-a",
	}
	entry := &sandboxEntry{state: initialState, sbx: sbx}
	m.sandboxes.Store("sandbox-a", entry)
	return m, entry, sbx
}

type recordingBootPreparer struct {
	released   []ReleaseBootRequest
	releaseErr error
	prepared   PreparedBoot
}

func (r *recordingBootPreparer) Prepare(context.Context, PrepareBootRequest) (PreparedBoot, error) {
	return r.prepared, nil
}

func (r *recordingBootPreparer) Release(_ context.Context, req ReleaseBootRequest) error {
	r.released = append(r.released, req)
	return r.releaseErr
}

func TestDeleteMissingSandboxReturnsNotFoundWithoutReleasingBootLayout(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot}

	if err := m.Delete(DeleteRequest{SandboxID: "sandbox-a"}); err == nil || err.Error() != "sandbox sandbox-a not found" {
		t.Fatalf("Delete() error = %v, want sandbox not found", err)
	}
	if len(boot.released) != 0 {
		t.Fatalf("released boot layouts = %#v, want none", boot.released)
	}
}

func TestCleanupStaleBootResourcesReleasesBootLayout(t *testing.T) {
	boot := &recordingBootPreparer{}
	m := &Manager{boot: boot}

	if err := m.cleanupStaleBootResources(context.Background(), []string{"sandbox-a"}); err != nil {
		t.Fatalf("cleanupStaleBootResources() error = %v", err)
	}
	if len(boot.released) != 1 || boot.released[0].SandboxID != "sandbox-a" {
		t.Fatalf("released boot layouts = %#v", boot.released)
	}
}

func TestCleanupStaleBootResourcesReturnsBootReleaseError(t *testing.T) {
	wantErr := errors.New("release failed")
	boot := &recordingBootPreparer{releaseErr: wantErr}
	m := &Manager{boot: boot}

	if err := m.cleanupStaleBootResources(context.Background(), []string{"sandbox-a"}); !errors.Is(err, wantErr) {
		t.Fatalf("cleanupStaleBootResources() error = %v, want %v", err, wantErr)
	}
}

func TestReserveSandboxEntrySerializesSameSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	defer m.sandboxes.CompareAndDelete(key, entry)

	done := make(chan error, 1)
	go func() {
		_, _, err := m.reserveSandboxEntry("sandbox-a")
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("same sandbox reserve completed while entry lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	entry.mu.Unlock()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("same sandbox reserve succeeded while an entry already existed")
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("same sandbox reserve error = %v, want already exists", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("same sandbox reserve did not unblock after entry lock was released")
	}
}
