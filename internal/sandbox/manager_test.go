package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/apperror"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/id"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/vmm"
	"github.com/openeuler/Conch/internal/webhook"
)

func TestCheckpointCapturesRunningAndSuspendedSandbox(t *testing.T) {
	tests := []struct {
		name            string
		initialState    State
		wantPauseBefore bool
	}{
		{name: "running", initialState: StateReady, wantPauseBefore: true},
		{name: "suspended", initialState: StateSuspended, wantPauseBefore: false},
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

			got, err := m.captureCheckpoint(context.Background(), "sandbox-a", entry)
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
		initialState State
	}{
		{name: "running", initialState: StateReady},
		{name: "suspended", initialState: StateSuspended},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := &recordingCheckpointCapture{err: errCapture}
			m, entry, _ := checkpointTestManager(tt.initialState, capture)

			_, err := m.captureCheckpoint(context.Background(), "sandbox-a", entry)
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
	m, entry, _ := checkpointTestManager(StateReady, capture)

	_, err := m.captureCheckpoint(context.Background(), "sandbox-a", entry)
	if !errors.Is(err, ErrCheckpointResume) || !errors.Is(err, errResume) {
		t.Fatalf("Checkpoint() error = %v, want joined resume failure", err)
	}
	if entry.state != StateSuspended {
		t.Fatalf("entry state after resume failure = %s, want %s", entry.state, StateSuspended)
	}
}

type recordingCheckpointCapture struct {
	requests []RuntimeCaptureRequest
	result   CapturedBootComponents
	err      error
}

func (r *recordingCheckpointCapture) Capture(_ context.Context, req RuntimeCaptureRequest) (CapturedBootComponents, error) {
	r.requests = append(r.requests, req)
	return r.result, r.err
}

func checkpointTestManager(initialState State, capture CheckpointCapture) (*Manager, *sandboxEntry, *Sandbox) {
	store := newMemorySandboxStore()
	req := testCreateRequest()
	_, _ = store.Create(context.Background(), Record{ID: req.SandboxID, State: initialState, CheckpointHeadTemplateID: req.TemplateID})
	m := &Manager{store: store, checkpointCapture: capture, requestTimeout: time.Second}
	sbx := &Sandbox{
		sandboxID: "sandbox-a",
	}
	entry := &sandboxEntry{state: initialState, sbx: sbx}
	m.sandboxes.Store("sandbox-a", entry)
	return m, entry, sbx
}

type recordingBootPreparer struct {
	released           []ReleaseBootRequest
	releaseErr         error
	releaseHook        func(context.Context) error
	releaseErrors      []error
	prepared           PreparedBoot
	prepareHook        func()
	releaseContextErr  error
	releaseHasDeadline bool
}

func (r *recordingBootPreparer) Prepare(context.Context, PrepareBootRequest) (PreparedBoot, error) {
	if r.prepareHook != nil {
		r.prepareHook()
	}
	return r.prepared, nil
}

func (r *recordingBootPreparer) Release(ctx context.Context, req ReleaseBootRequest) error {
	r.released = append(r.released, req)
	if r.releaseHook != nil {
		return r.releaseHook(ctx)
	}
	r.releaseContextErr = ctx.Err()
	_, r.releaseHasDeadline = ctx.Deadline()
	if len(r.releaseErrors) > 0 {
		err := r.releaseErrors[0]
		r.releaseErrors = r.releaseErrors[1:]
		return err
	}
	return r.releaseErr
}

type blockingBootPreparer struct {
	entered chan struct{}
	leaseID string
}

func (b *blockingBootPreparer) Prepare(ctx context.Context, _ PrepareBootRequest) (PreparedBoot, error) {
	b.leaseID, _ = leases.FromContext(ctx)
	close(b.entered)
	<-ctx.Done()
	return PreparedBoot{}, ctx.Err()
}

func (b *blockingBootPreparer) Release(context.Context, ReleaseBootRequest) error { return nil }

func testCreateRequest() CreateRequest {
	return CreateRequest{SandboxID: "sandbox-a", TemplateID: digest.FromString("template").String(), TemplateName: "example:latest", VMMName: "cloud-hypervisor", VCPUNum: 2, VCPUMax: 2, RAMMB: 512}
}

func TestReserveCreateRAMAllowsOnlyOneConcurrentRequestAtCapacity(t *testing.T) {
	m := &Manager{memoryOvercommit: 1.0, memorySafetyMB: 1024}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- m.reserveCreateRAMWithAvailable(4096, 5120)
		}()
	}
	close(start)

	var succeeded, exhausted int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrResourceExhausted):
			exhausted++
		default:
			t.Fatalf("reserveCreateRAM() error = %v", err)
		}
	}
	if succeeded != 1 || exhausted != 1 {
		t.Fatalf("results = %d succeeded, %d exhausted; want 1 each", succeeded, exhausted)
	}
	m.pendingCreateRAM.Add(-4096)
	if pending := m.pendingCreateRAM.Load(); pending != 0 {
		t.Fatalf("pendingCreateRAM after release = %d, want 0", pending)
	}
}

func newLifecycleTestManager(t *testing.T, boot BootPreparer) (*Manager, *memorySandboxStore, *testLeases) {
	t.Helper()
	old := config.WorkDir
	config.WorkDir = t.TempDir()
	t.Cleanup(func() { config.WorkDir = old })
	leaseStore := &testLeases{items: make(map[string]leases.Lease), resources: make(map[string][]leases.Resource)}
	contentStore, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := containerd.New("", containerd.WithServices(containerd.WithLeasesService(leaseStore), containerd.WithContentStore(contentStore)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := newMemorySandboxStore()
	m := &Manager{context: ctx, store: store, client: &containerdclient.Client{Client: client}, boot: boot,
		cidAllocator: NewCIDAllocator(), requestTimeout: time.Second,
		vmmBinaries: map[string]string{"cloud-hypervisor": "/unused"}, checkpointCapture: NewFullCheckpointCapture(),
	}
	// The launch fixture supplies only an IP placeholder, not an allocated slot.
	placeholder := &netstack.Slot{}
	m.launch = func(context.Context, CreateRequest, VMStartSpec, createRuntimeIDs, bool) (*Sandbox, error) {
		return &Sandbox{process: &vmm.Process{}, slot: placeholder}, nil
	}
	store.beforeUpdate = func() {
		if value, ok := m.sandboxes.Load(testCreateRequest().SandboxID); ok {
			if sbx := value.(*sandboxEntry).sbx; sbx != nil && sbx.slot == placeholder {
				sbx.slot = nil
			}
		}
	}
	return m, store, leaseStore
}

type testLeases struct {
	leases.Manager
	mu        sync.Mutex
	items     map[string]leases.Lease
	resources map[string][]leases.Resource
	deleteErr error
	onDelete  func(string)
}

func (s *testLeases) Create(ctx context.Context, opts ...leases.Opt) (leases.Lease, error) {
	if err := ctx.Err(); err != nil {
		return leases.Lease{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lease := leases.Lease{}
	for _, opt := range opts {
		if err := opt(&lease); err != nil {
			return lease, err
		}
	}
	if _, ok := s.items[lease.ID]; ok {
		return lease, errdefs.ErrAlreadyExists
	}
	s.items[lease.ID] = lease
	return lease, nil
}
func (s *testLeases) Delete(ctx context.Context, lease leases.Lease, _ ...leases.DeleteOpt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.onDelete != nil {
		s.onDelete(lease.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if _, ok := s.items[lease.ID]; !ok {
		return errdefs.ErrNotFound
	}
	delete(s.items, lease.ID)
	delete(s.resources, lease.ID)
	return nil
}
func (s *testLeases) AddResource(ctx context.Context, lease leases.Lease, r leases.Resource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[lease.ID]; !ok {
		return fmt.Errorf("missing lease %s", lease.ID)
	}
	s.resources[lease.ID] = append(s.resources[lease.ID], r)
	return nil
}
func (s *testLeases) List(context.Context, ...string) ([]leases.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]leases.Lease, 0, len(s.items))
	for _, l := range s.items {
		out = append(out, l)
	}
	return out, nil
}
func (s *testLeases) ListResources(_ context.Context, l leases.Lease) ([]leases.Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]leases.Resource(nil), s.resources[l.ID]...), nil
}

type observingBoot struct {
	recordingBootPreparer
	prepare func(context.Context, PrepareBootRequest) (PreparedBoot, error)
}

func (b *observingBoot) Prepare(ctx context.Context, req PrepareBootRequest) (PreparedBoot, error) {
	return b.prepare(ctx, req)
}

func readyBoot() *recordingBootPreparer {
	return &recordingBootPreparer{prepared: PreparedBoot{Spec: VMStartSpec{MemorySizeMB: 512}, RuntimeSnapshots: []SnapshotRef{
		{Snapshotter: "erofs", Role: "rootfs", Key: "sandbox-a"},
		{Snapshotter: "erofs", Role: "vm", Key: "view-vm-sandbox-a"},
	}}}
}

func TestCreateOwnsPersistenceAndLease(t *testing.T) {
	ctx := context.Background()
	b := &observingBoot{}
	m, store, leaseStore := newLifecycleTestManager(t, b)
	calls := 0
	b.prepare = func(ctx context.Context, _ PrepareBootRequest) (PreparedBoot, error) {
		calls++
		leaseID, ok := leases.FromContext(ctx)
		if !ok || leaseID != createLeaseID("sandbox-a") {
			t.Fatalf("prepare lease = %q", leaseID)
		}
		rec, err := store.Get(ctx, "sandbox-a")
		if err != nil || rec.State != StateCreating {
			t.Fatalf("prepare record = %#v, %v", rec, err)
		}
		return readyBoot().prepared, nil
	}
	leaseStore.onDelete = func(string) {
		rec, err := store.Get(ctx, "sandbox-a")
		if err != nil || rec.State != StateReady || len(rec.RuntimeSnapshots) != 2 {
			t.Errorf("record at lease release = %#v, %v", rec, err)
		}
	}
	result, err := m.Create(ctx, testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("boot prepares = %d", calls)
	}
	if result.TemplateID != testCreateRequest().TemplateID || result.TemplateName != "example:latest" || result.AgentToken == "" || result.RamMB != 512 {
		t.Fatalf("result = %#v", result)
	}
	if got := store.operationLog(); !reflect.DeepEqual(got, []string{"create:CREATING", "update:READY"}) {
		t.Fatalf("operations = %#v", got)
	}
	remaining, _ := leaseStore.List(ctx)
	if len(remaining) != 0 {
		t.Fatalf("remaining leases = %#v", remaining)
	}
}

func TestCreateFailureCleanupAndRetention(t *testing.T) {
	for _, stage := range []string{"prepare", "launch", "ready"} {
		for _, cleanupFails := range []bool{false, true} {
			t.Run(stage+"/retain="+map[bool]string{true: "true", false: "false"}[cleanupFails], func(t *testing.T) {
				primary := errors.New("operation failed")
				cleanupErr := errors.New("boot release failed")
				b := &observingBoot{}
				b.prepare = func(context.Context, PrepareBootRequest) (PreparedBoot, error) {
					if stage == "prepare" {
						return PreparedBoot{}, primary
					}
					return readyBoot().prepared, nil
				}
				if cleanupFails {
					b.releaseErr = cleanupErr
				}
				m, store, leaseStore := newLifecycleTestManager(t, b)
				if stage == "launch" {
					m.launch = func(context.Context, CreateRequest, VMStartSpec, createRuntimeIDs, bool) (*Sandbox, error) {
						return nil, primary
					}
				}
				if stage == "ready" {
					store.updateHook = func(rec Record) error {
						if rec.State == StateReady {
							return primary
						}
						return nil
					}
				}
				_, err := m.Create(context.Background(), testCreateRequest())
				if !errors.Is(err, primary) {
					t.Fatalf("Create = %v", err)
				}
				if len(b.released) != 1 || b.releaseContextErr != nil || !b.releaseHasDeadline {
					t.Fatalf("cleanup = %#v", b)
				}
				rec, getErr := store.Get(context.Background(), "sandbox-a")
				remaining, _ := leaseStore.List(context.Background())
				if cleanupFails {
					if getErr != nil || rec.State != StateCreating || !strings.Contains(rec.LastError, cleanupErr.Error()) || len(remaining) != 1 {
						t.Fatalf("retained record=%#v leases=%#v err=%v", rec, remaining, getErr)
					}
				} else {
					if !errors.Is(getErr, ErrNotFound) || len(remaining) != 0 {
						t.Fatalf("record=%#v leases=%#v err=%v", rec, remaining, getErr)
					}
					if _, ok := m.sandboxes.Load("sandbox-a"); ok {
						t.Fatal("failed instance remains")
					}
				}
			})
		}
	}
}

func TestCreateCancellationCleansPartialBootWithLiveContext(t *testing.T) {
	b := &blockingBootPreparer{entered: make(chan struct{})}
	observed := &observingBoot{prepare: b.Prepare}
	m, store, leaseStore := newLifecycleTestManager(t, observed)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.Create(ctx, testCreateRequest()); done <- err }()
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("prepare not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Create = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Create blocked")
	}
	if observed.releaseContextErr != nil || !observed.releaseHasDeadline || len(observed.released) != 1 {
		t.Fatalf("cleanup = %#v", observed)
	}
	if _, err := store.Get(context.Background(), "sandbox-a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if remaining, _ := leaseStore.List(context.Background()); len(remaining) != 0 {
		t.Fatal(remaining)
	}
}

func TestCreateRejectsInvalidRequestsBeforeResourceAllocation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CreateRequest)
		want   error
	}{
		{"id", func(r *CreateRequest) { r.SandboxID = "../escape" }, ErrInvalidArgument},
		{"memory snapshot id", func(r *CreateRequest) { r.SandboxID = "foo-mem" }, ErrInvalidArgument},
		{"rootfs view id", func(r *CreateRequest) { r.SandboxID = "view-rootfs-foo" }, ErrInvalidArgument},
		{"memory view id", func(r *CreateRequest) { r.SandboxID = "view-mem-foo" }, ErrInvalidArgument},
		{"vm view id", func(r *CreateRequest) { r.SandboxID = "view-vm-foo" }, ErrInvalidArgument},
		{"padded reserved id", func(r *CreateRequest) { r.SandboxID = " foo-mem " }, ErrInvalidArgument},
		{"template", func(r *CreateRequest) { r.TemplateID = "not-a-digest" }, ErrInvalidArgument},
		{"vmm", func(r *CreateRequest) { r.VMMName = "unknown" }, ErrInvalidArgument},
		{"cpu", func(r *CreateRequest) { r.VCPUMax = 1 }, ErrInvalidArgument},
		{"memory", func(r *CreateRequest) { r.RAMMB = 0 }, ErrInvalidArgument},
		{"limit", func(r *CreateRequest) { r.RAMMB = runtimeapi.SandboxMaxRAMMB + 1 }, ErrResourceExhausted},
		{"environment", func(r *CreateRequest) { r.Env = map[string]string{"BAD=KEY": "x"} }, ErrInvalidEnvironment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, store, _ := newLifecycleTestManager(t, readyBoot())
			req := testCreateRequest()
			tc.mutate(&req)
			_, err := m.Create(context.Background(), req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Create = %v", err)
			}
			if len(store.operationLog()) != 0 {
				t.Fatal("invalid request wrote state")
			}
		})
	}
}

func TestValidateSandboxIDAllowsNamesOutsideReservedPatterns(t *testing.T) {
	for _, sandboxID := range []string{"foo", "foo-mem-extra", "my-view-vm-foo", "view-vm", "foo-MEM", "View-vm-foo"} {
		t.Run(sandboxID, func(t *testing.T) {
			if err := validateSandboxID(sandboxID); err != nil {
				t.Fatalf("validateSandboxID(%q): %v", sandboxID, err)
			}
		})
	}
}

func TestCreateReservesPersistentIDAndGeneratesID(t *testing.T) {
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	req := testCreateRequest()
	_, _ = store.Create(context.Background(), Record{ID: req.SandboxID, State: StateCreating, SourceTemplateID: req.TemplateID})
	if _, err := m.Create(context.Background(), req); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate = %v", err)
	}
	req.SandboxID = ""
	result, err := m.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := id.Validate(result.SandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), result.SandboxID); err != nil {
		t.Fatal(err)
	}
}

func TestCombineOperationErrorsPreservesPrimaryClassification(t *testing.T) {
	primary := errors.New("state write failed")
	combined := combineOperationErrors(primary, ErrNotFound.New())
	var appErr *apperror.Error
	if !errors.Is(combined, primary) || errors.As(combined, &appErr) {
		t.Fatalf("classification changed: %v", combined)
	}
	combined = combineOperationErrors(ErrInvalidArgument.New(), ErrNotFound.New())
	if !errors.As(combined, &appErr) || appErr.Code() != ErrInvalidArgument.New().Code() {
		t.Fatalf("classification changed: %v", combined)
	}
}

func TestCreateFailureKeepsRecoveryRecordWhenLeaseReleaseFails(t *testing.T) {
	b := &observingBoot{prepare: func(context.Context, PrepareBootRequest) (PreparedBoot, error) {
		return PreparedBoot{}, errors.New("prepare failed")
	}}
	m, store, ls := newLifecycleTestManager(t, b)
	ls.deleteErr = errors.New("lease service unavailable")
	if _, err := m.Create(context.Background(), testCreateRequest()); err == nil {
		t.Fatal("Create succeeded")
	}
	rec, err := store.Get(context.Background(), "sandbox-a")
	if err != nil || rec.State != StateCreating {
		t.Fatalf("recovery record=%#v, %v", rec, err)
	}
	ls.deleteErr = nil
	if err := m.Delete(context.Background(), "sandbox-a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ls.List(context.Background()); len(got) != 0 {
		t.Fatal(got)
	}
}

func seedRuntime(t *testing.T, m *Manager, store *memorySandboxStore, cleanup func(context.Context) error) *sandboxEntry {
	t.Helper()
	req := testCreateRequest()
	_, err := store.Create(context.Background(), Record{ID: req.SandboxID, State: StateReady, SourceTemplateID: req.TemplateID, CheckpointHeadTemplateID: req.TemplateID, RuntimeSnapshots: readyBoot().prepared.RuntimeSnapshots})
	if err != nil {
		t.Fatal(err)
	}
	entry := &sandboxEntry{state: StateReady}
	m.boot = &recordingBootPreparer{releaseHook: cleanup}
	m.sandboxes.Store(req.SandboxID, entry)
	return entry
}

func TestDeleteAndExitShareLifecycleLock(t *testing.T) {
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	entered, proceed := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	entry := seedRuntime(t, m, store, func(context.Context) error { calls.Add(1); close(entered); <-proceed; return nil })
	deleted := make(chan error, 1)
	go func() { deleted <- m.Delete(context.Background(), "sandbox-a") }()
	<-entered
	exited := make(chan struct{})
	go func() { m.handleSandboxExit("sandbox-a", entry); close(exited) }()
	close(proceed)
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Delete blocked")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("exit handler blocked")
	}
	if calls.Load() != 1 {
		t.Fatalf("cleanup calls = %d", calls.Load())
	}
	if _, err := store.Get(context.Background(), "sandbox-a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), "sandbox-a"); err != nil {
		t.Fatalf("repeated Delete = %v", err)
	}
}

func TestDeleteRetainsRecordOnCleanupFailure(t *testing.T) {
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	want := errors.New("VMM still running")
	seedRuntime(t, m, store, func(context.Context) error { return want })
	if err := m.Delete(context.Background(), "sandbox-a"); !errors.Is(err, want) {
		t.Fatalf("Delete = %v", err)
	}
	rec, err := store.Get(context.Background(), "sandbox-a")
	if err != nil || rec.State != StateUnknown || len(rec.RuntimeSnapshots) != 2 || !strings.Contains(rec.LastError, want.Error()) {
		t.Fatalf("record = %#v, %v", rec, err)
	}
}

func TestDeleteWithoutEntryOnlyFinalizesRecordAndLease(t *testing.T) {
	b := readyBoot()
	m, store, ls := newLifecycleTestManager(t, b)
	req := testCreateRequest()
	_, _ = store.Create(context.Background(), Record{ID: req.SandboxID, State: StateCreating, SourceTemplateID: req.TemplateID})
	lease, err := m.client.LeasesService().Create(context.Background(), func(l *leases.Lease) error { l.ID = createLeaseID(req.SandboxID); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), req.SandboxID); err != nil {
		t.Fatal(err)
	}
	if len(b.released) != 0 {
		t.Fatalf("boot release count = %d", len(b.released))
	}
	if got, _ := ls.List(context.Background()); len(got) != 0 {
		t.Fatalf("lease %s remains", lease.ID)
	}
}

func TestOldExitCannotChangeReplacementSandbox(t *testing.T) {
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	old := &sandboxEntry{}
	seedRuntime(t, m, store, func(context.Context) error { return nil })
	m.handleSandboxExit("sandbox-a", old)
	rec, err := store.Get(context.Background(), "sandbox-a")
	if err != nil || rec.State != StateReady {
		t.Fatalf("replacement record = %#v, %v", rec, err)
	}
}

func TestLifecycleEventsFollowPersistence(t *testing.T) {
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	events := make(chan webhook.Event, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event webhook.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		} else {
			events <- event
		}
	}))
	defer server.Close()
	dispatcher := webhook.NewDispatcher()
	_, err := dispatcher.Create(runtimeapi.WebhookCreateOptions{Name: "test", URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	m.WebhookDispatcher = dispatcher
	if _, err := m.Create(context.Background(), testCreateRequest()); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Type != webhook.EventSandboxCreated {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing created event")
	}
	value, _ := m.sandboxes.Load("sandbox-a")
	entry := value.(*sandboxEntry)
	m.handleSandboxExit("sandbox-a", entry)
	m.handleSandboxExit("sandbox-a", entry)
	select {
	case event := <-events:
		if event.EventData.KillReason != "orphaned" {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing exit event")
	}
	rec, err := store.Get(context.Background(), "sandbox-a")
	if err != nil || rec.State != StateUnknown || len(rec.RuntimeSnapshots) != 0 {
		t.Fatalf("exit record = %#v, %v", rec, err)
	}
	if err := m.Delete(context.Background(), "sandbox-a"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("duplicate event = %#v", event)
	default:
	}
}

func TestDifferentSandboxOperationsDoNotBlock(t *testing.T) {
	var locks sandboxLifecycleLocks
	unlock := locks.lock("a")
	defer unlock()
	done := make(chan struct{})
	go func() { release := locks.lock("b"); release(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unrelated sandbox blocked")
	}
}

// Exercise the real VMM API calls with a local control socket; no VM is booted.
func TestSuspendResumePersistStateAndReturnWriteErrors(t *testing.T) {
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	entry := seedRuntime(t, m, store, func(context.Context) error { return nil })
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
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(int(status.Load()))
	})}
	go server.Serve(listener)
	defer server.Close()
	entry.sbx = &Sandbox{process: process}
	ctx := context.Background()
	if err := m.Suspend(ctx, "sandbox-a"); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.Get(ctx, "sandbox-a")
	if rec.State != StateSuspended || entry.state != StateSuspended {
		t.Fatalf("suspend state=%s runtime=%s", rec.State, entry.state)
	}
	if err := m.Resume(ctx, "sandbox-a"); err != nil {
		t.Fatal(err)
	}
	rec, _ = store.Get(ctx, "sandbox-a")
	if rec.State != StateReady || entry.state != StateReady {
		t.Fatalf("resume state=%s runtime=%s", rec.State, entry.state)
	}
	want := errors.New("persist state failed")
	store.updateHook = func(Record) error { return want }
	if err := m.Suspend(ctx, "sandbox-a"); !errors.Is(err, want) {
		t.Fatalf("state write error hidden: %v", err)
	}
	if entry.state != StateSuspended {
		t.Fatalf("actual runtime state=%s", entry.state)
	}
	store.updateHook = nil
	if err := m.Resume(ctx, "sandbox-a"); err != nil {
		t.Fatal(err)
	}
	status.Store(http.StatusInternalServerError)
	if err := m.Suspend(ctx, "sandbox-a"); err == nil {
		t.Fatal("VMM failure accepted")
	}
	rec, _ = store.Get(ctx, "sandbox-a")
	if rec.State != StateUnknown || rec.LastError == "" {
		t.Fatalf("failed VMM state=%#v", rec)
	}
}

func checkpointFixture(t *testing.T) (*Manager, *memorySandboxStore, *testLeases, *recordingCheckpointCapture) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required for checkpoint content")
	}
	m, store, leaseStore := newLifecycleTestManager(t, readyBoot())
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
	target, err := conchimage.BuildBootIndexInContent(context.Background(), m.client.ContentStore(), conchimage.BootIndexContentOptions{RootfsDescriptor: components[0], SandboxDescriptor: components[1]})
	if err != nil {
		t.Fatal(err)
	}
	req := testCreateRequest()
	_, err = store.Create(context.Background(), Record{ID: req.SandboxID, State: StateReady, CheckpointHeadTemplateID: target.Digest.String()})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mem.img"), []byte("memory"), 0600); err != nil {
		t.Fatal(err)
	}
	capture := &recordingCheckpointCapture{result: CapturedBootComponents{MemRootPath: dir, VMMName: "cloud-hypervisor", MemorySizeMB: 512}}
	m.checkpointCapture = capture
	m.sandboxes.Store(req.SandboxID, &sandboxEntry{state: StateReady, sbx: &Sandbox{}})
	return m, store, leaseStore, capture
}

func TestCheckpointPublishesHeadAndProtectsParentDuringRegistration(t *testing.T) {
	m, store, leaseStore, capture := checkpointFixture(t)
	ctx := context.Background()
	original, _ := store.Get(ctx, "sandbox-a")
	called := false
	result, err := m.Checkpoint(ctx, "sandbox-a", func(ctx context.Context, result CheckpointResult) error {
		called = true
		rec, err := store.Get(ctx, "sandbox-a")
		if err != nil || rec.CheckpointHeadTemplateID != result.BootIndexDigest {
			t.Fatalf("checkpoint record=%#v, %v", rec, err)
		}
		if result.ParentBootIndexDigest != original.CheckpointHeadTemplateID {
			t.Fatal(result)
		}
		leaseID, ok := leases.FromContext(ctx)
		if !ok {
			t.Fatal("registration has no lease")
		}
		resources, _ := leaseStore.ListResources(ctx, leases.Lease{ID: leaseID})
		if len(resources) != 1 || resources[0].ID != original.CheckpointHeadTemplateID {
			t.Fatalf("parent resources=%#v", resources)
		}
		info, err := conchimage.InspectBootIndexContent(ctx, m.client.ContentStore(), result.Target)
		if err != nil || !info.Resume || info.MemorySizeMB != 512 {
			t.Fatalf("published=%#v, %v", info, err)
		}
		return nil
	})
	if err != nil || !called || result.BootIndexDigest == original.CheckpointHeadTemplateID {
		t.Fatalf("Checkpoint=%#v, %v", result, err)
	}
	if remaining, _ := leaseStore.List(ctx); len(remaining) != 0 {
		t.Fatalf("leases=%#v", remaining)
	}
	if _, err := os.Stat(capture.result.MemRootPath); !os.IsNotExist(err) {
		t.Fatalf("capture directory remains: %v", err)
	}
}

func TestCheckpointRegistrationFailureRollsBackHead(t *testing.T) {
	m, store, ls, _ := checkpointFixture(t)
	ctx := context.Background()
	original, _ := store.Get(ctx, "sandbox-a")
	want := errors.New("template registration failed")
	_, err := m.Checkpoint(ctx, "sandbox-a", func(context.Context, CheckpointResult) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("Checkpoint=%v", err)
	}
	rec, _ := store.Get(ctx, "sandbox-a")
	if rec.CheckpointHeadTemplateID != original.CheckpointHeadTemplateID {
		t.Fatalf("head=%s", rec.CheckpointHeadTemplateID)
	}
	if remaining, _ := ls.List(ctx); len(remaining) != 0 {
		t.Fatal(remaining)
	}
}

func TestCheckpointPersistenceFailureSkipsRegistration(t *testing.T) {
	m, store, _, _ := checkpointFixture(t)
	want := errors.New("head persistence failed")
	store.updateHook = func(Record) error { return want }
	_, err := m.Checkpoint(context.Background(), "sandbox-a", func(context.Context, CheckpointResult) error {
		t.Fatal("registered before state persisted")
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("Checkpoint=%v", err)
	}
}

func TestCheckpointMissingContentFailsBeforeCapture(t *testing.T) {
	m, store, _, capture := checkpointFixture(t)
	rec, _ := store.Get(context.Background(), "sandbox-a")
	info, _ := conchimage.InspectBootIndex(context.Background(), m.client, rec.CheckpointHeadTemplateID)
	if err := m.client.ContentStore().Delete(context.Background(), info.RootfsDescriptor.Digest); err != nil {
		t.Fatal(err)
	}
	_, err := m.Checkpoint(context.Background(), "sandbox-a", func(context.Context, CheckpointResult) error { t.Fatal("registered missing content"); return nil })
	if !errors.Is(err, ErrFailedPrecondition) || len(capture.requests) != 0 {
		t.Fatalf("Checkpoint=%v captures=%d", err, len(capture.requests))
	}
}

func TestCheckpointSerializesDeleteUntilRegistrationCompletes(t *testing.T) {
	m, _, _, _ := checkpointFixture(t)
	entered, proceed := make(chan struct{}), make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		_, err := m.Checkpoint(context.Background(), "sandbox-a", func(context.Context, CheckpointResult) error { close(entered); <-proceed; return nil })
		completed <- err
	}()
	select {
	case <-entered:
	case err := <-completed:
		t.Fatalf("checkpoint ended before registration: %v", err)
	case <-time.After(time.Second):
		t.Fatal("registration not reached")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- m.Delete(context.Background(), "sandbox-a") }()
	select {
	case err := <-deleted:
		t.Fatalf("Delete passed checkpoint lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(proceed)
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRetriesIdempotentResourcesAndRetainsDependencies(t *testing.T) {
	b := readyBoot()
	m, _, _ := newLifecycleTestManager(t, b)
	_, _ = m.AllocateUniqueCID("sandbox-a")
	entry := &sandboxEntry{}
	b.releaseErr = errors.New("boot release failed")
	if err := m.cleanupSandbox(context.Background(), "sandbox-a", entry); !errors.Is(err, b.releaseErr) {
		t.Fatal(err)
	}
	if m.cidAllocator.GetActiveCount() != 1 {
		t.Fatal("released CID before boot cleanup succeeded")
	}
	b.releaseErr = nil
	for range 2 {
		if err := m.cleanupSandbox(context.Background(), "sandbox-a", entry); err != nil {
			t.Fatal(err)
		}
	}
	if len(b.released) != 3 || m.cidAllocator.GetActiveCount() != 0 {
		t.Fatal("idempotent cleanup did not finish")
	}
}

type memorySandboxStore struct {
	mu           sync.Mutex
	records      map[string]Record
	operations   []string
	beforeUpdate func()
	updateHook   func(Record) error
	deleteHook   func() error
}

func newMemorySandboxStore() *memorySandboxStore {
	return &memorySandboxStore{records: make(map[string]Record)}
}

func (s *memorySandboxStore) Create(_ context.Context, record Record) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateMemorySandboxRecord(record); err != nil {
		return Record{}, err
	}
	if _, ok := s.records[record.ID]; ok {
		return Record{}, ErrAlreadyExists.New()
	}
	if record.CreatedAt == 0 {
		record.CreatedAt = time.Now().UnixNano()
	}
	s.records[record.ID] = record
	s.operations = append(s.operations, "create:"+string(record.State))
	return record, nil
}

func (s *memorySandboxStore) Update(_ context.Context, record Record) (Record, error) {
	if s.beforeUpdate != nil {
		s.beforeUpdate()
	}
	if s.updateHook != nil {
		if err := s.updateHook(record); err != nil {
			return Record{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateMemorySandboxRecord(record); err != nil {
		return Record{}, err
	}
	if _, ok := s.records[record.ID]; !ok {
		return Record{}, ErrNotFound.New()
	}
	s.records[record.ID] = record
	s.operations = append(s.operations, "update:"+string(record.State))
	return record, nil
}

func validateMemorySandboxRecord(record Record) error {
	if record.State == StateCreating {
		if record.SourceTemplateID == "" {
			return ErrInvalidArgument.New()
		}
		return nil
	}
	if record.CheckpointHeadTemplateID == "" {
		return ErrInvalidArgument.New()
	}
	return nil
}

func (s *memorySandboxStore) Get(_ context.Context, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return Record{}, ErrNotFound.New()
	}
	return record, nil
}

func (s *memorySandboxStore) List(_ context.Context, filter Filter) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]Record, 0, len(s.records))
	for _, record := range s.records {
		if filter.State == "" || record.State == filter.State {
			records = append(records, record)
		}
	}
	return records, nil
}

func (s *memorySandboxStore) Delete(_ context.Context, id string) error {
	if s.deleteHook != nil {
		if err := s.deleteHook(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, id)
	s.operations = append(s.operations, "delete")
	return nil
}

func (s *memorySandboxStore) operationLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.operations...)
}

func (s *memorySandboxStore) Put(ctx context.Context, record Record) error {
	if _, err := s.Get(ctx, record.ID); errors.Is(err, ErrNotFound) {
		_, err = s.Create(ctx, record)
		return err
	}
	_, err := s.Update(ctx, record)
	return err
}

func TestDeleteFinalizationFailureRetainsProgress(t *testing.T) {
	for _, stage := range []string{"lease", "record"} {
		t.Run(stage, func(t *testing.T) {
			m, store, ls := newLifecycleTestManager(t, readyBoot())
			calls := 0
			entry := seedRuntime(t, m, store, func(context.Context) error { calls++; return nil })
			want := errors.New(stage + " failed")
			if stage == "lease" {
				ls.deleteErr = want
			} else {
				store.deleteHook = func() error { return want }
			}
			if err := m.Delete(context.Background(), "sandbox-a"); !errors.Is(err, want) {
				t.Fatal(err)
			}
			value, ok := m.sandboxes.Load("sandbox-a")
			rec, err := store.Get(context.Background(), "sandbox-a")
			if !ok || value != entry || entry.state != StateUnknown || err != nil || !strings.Contains(rec.LastError, want.Error()) {
				t.Fatalf("entry=%v record=%#v err=%v", ok, rec, err)
			}
			m.handleSandboxExit("sandbox-a", entry)
			if calls != 1 {
				t.Fatal("exit repeated cleanup")
			}
			ls.deleteErr, store.deleteHook = nil, nil
			if err := m.Delete(context.Background(), "sandbox-a"); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatal("retry did not invoke idempotent cleanup")
			}
			if _, ok := m.sandboxes.Load("sandbox-a"); ok {
				t.Fatal("entry remains")
			}
		})
	}
}

func TestDeleteReportsFailureToPersistError(t *testing.T) {
	m, store, ls := newLifecycleTestManager(t, readyBoot())
	seedRuntime(t, m, store, func(context.Context) error { return nil })
	ls.deleteErr = errors.New("lease failure")
	store.updateHook = func(Record) error { return errors.New("status failure") }
	err := m.Delete(context.Background(), "sandbox-a")
	if !errors.Is(err, ls.deleteErr) || !strings.Contains(err.Error(), "status failure") {
		t.Fatal(err)
	}
}

func TestDeleteCleanupContinuesAfterRequestCancellation(t *testing.T) {
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seedRuntime(t, m, store, func(cleanupCtx context.Context) error {
		cancel()
		if _, ok := cleanupCtx.Deadline(); !ok {
			t.Fatal("cleanup has no deadline")
		}
		return cleanupCtx.Err()
	})
	if err := m.Delete(ctx, "sandbox-a"); err != nil {
		t.Fatal(err)
	}
}

func TestStartupRecoveryRetriesIdempotentBootCleanup(t *testing.T) {
	b := readyBoot()
	m, store, ls := newLifecycleTestManager(t, b)
	req := testCreateRequest()
	_, _ = store.Create(context.Background(), Record{ID: req.SandboxID, State: StateCreating, SourceTemplateID: req.TemplateID})
	ls.deleteErr = errors.New("lease failure")
	if err := m.recoverStaleSandbox(context.Background(), req.SandboxID); !errors.Is(err, ls.deleteErr) {
		t.Fatal(err)
	}
	ls.deleteErr = nil
	if err := m.recoverStaleSandbox(context.Background(), req.SandboxID); err != nil {
		t.Fatal(err)
	}
	if len(b.released) != 2 {
		t.Fatalf("boot releases=%d", len(b.released))
	}
	if _, err := store.Get(context.Background(), req.SandboxID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestCreateRecordDeleteFailureRetainsEntry(t *testing.T) {
	b := readyBoot()
	m, store, _ := newLifecycleTestManager(t, b)
	m.launch = func(context.Context, CreateRequest, VMStartSpec, createRuntimeIDs, bool) (*Sandbox, error) {
		return nil, errors.New("launch failure")
	}
	store.deleteHook = func() error { return errors.New("record failure") }
	if _, err := m.Create(context.Background(), testCreateRequest()); err == nil {
		t.Fatal("Create succeeded")
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); !ok {
		t.Fatal("lost entry")
	}
	store.deleteHook = nil
	if err := m.Delete(context.Background(), "sandbox-a"); err != nil {
		t.Fatal(err)
	}
	if len(b.released) != 2 {
		t.Fatal("boot cleanup was not retried")
	}
}

func TestCleanupRetainsProcessUntilSocketsAreRemoved(t *testing.T) {
	b := readyBoot()
	m, _, _ := newLifecycleTestManager(t, b)
	_, _ = m.AllocateUniqueCID("sandbox-a")
	socket := filepath.Join(t.TempDir(), "vmm.sock")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	process := &vmm.Process{VmmSocketPath: socket, VsockSocketPath: "invalid\x00socket"}
	entry := &sandboxEntry{sbx: &Sandbox{process: process}}
	if err := m.cleanupSandbox(context.Background(), "sandbox-a", entry); err == nil {
		t.Fatal("socket deletion unexpectedly succeeded")
	}
	if entry.sbx.process != process {
		t.Fatal("lost process paths after socket deletion failure")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("first socket was not removed: %v", err)
	}
	if len(b.released) != 0 || m.cidAllocator.GetActiveCount() != 1 {
		t.Fatal("released dependencies after socket deletion failure")
	}
	process.VsockSocketPath = filepath.Join(t.TempDir(), "vsock.sock")
	if err := os.WriteFile(process.VsockSocketPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.cleanupSandbox(context.Background(), "sandbox-a", entry); err != nil {
		t.Fatal(err)
	}
	if entry.sbx.process != nil {
		t.Fatal("process remains after socket cleanup")
	}
	if _, err := os.Stat(process.VsockSocketPath); !os.IsNotExist(err) {
		t.Fatalf("vsock socket remains: %v", err)
	}
	if len(b.released) != 1 || m.cidAllocator.GetActiveCount() != 0 {
		t.Fatal("cleanup did not finish")
	}
}
