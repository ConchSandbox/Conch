package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/opencontainers/go-digest"

	"github.com/openeuler/Conch/internal/envd"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

type fakeRouteTable struct {
	mu       sync.Mutex
	closed   bool
	cancels  map[string]context.CancelFunc
	routes   map[string]string
	masks    map[string]string
	inserted []string
	released []string
}

func newFakeRouteTable() *fakeRouteTable {
	return &fakeRouteTable{
		cancels: make(map[string]context.CancelFunc),
		routes:  make(map[string]string),
		masks:   make(map[string]string),
	}
}

func (f *fakeRouteTable) Insert(id string) context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	if cancel, ok := f.cancels[id]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancels[id] = cancel
	f.inserted = append(f.inserted, id)
	return ctx
}

func (f *fakeRouteTable) Publish(id, ip, maskRequestHost string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("registry closed")
	}
	if _, ok := f.cancels[id]; !ok {
		return errors.New("no route slot")
	}
	if current, ok := f.routes[id]; ok && current != ip {
		return errors.New("ip changed")
	}
	f.routes[id] = ip
	f.masks[id] = maskRequestHost
	return nil
}

func (f *fakeRouteTable) Release(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cancel, ok := f.cancels[id]; ok {
		cancel()
		delete(f.cancels, id)
	}
	delete(f.routes, id)
	delete(f.masks, id)
	f.released = append(f.released, id)
}

func (f *fakeRouteTable) routeIP(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ip, ok := f.routes[id]
	return ip, ok
}

func (f *fakeRouteTable) releaseCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, released := range f.released {
		if released == id {
			count++
		}
	}
	return count
}

type fakeCapacityLedger struct {
	mu       sync.Mutex
	reserved map[string]capacityReservation
	released []string
}

type capacityReservation struct {
	cpus, memoryMB int64
}

func (c *fakeCapacityLedger) Reserve(id string, cpus, memoryMB int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reserved == nil {
		c.reserved = make(map[string]capacityReservation)
	}
	c.reserved[id] = capacityReservation{cpus: cpus, memoryMB: memoryMB}
	return nil
}

func (c *fakeCapacityLedger) Release(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reserved, id)
	c.released = append(c.released, id)
}

func (c *fakeCapacityLedger) holding(id string) (capacityReservation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reservation, ok := c.reserved[id]
	return reservation, ok
}

func (c *fakeCapacityLedger) releaseCount(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, released := range c.released {
		if released == id {
			count++
		}
	}
	return count
}

type fakeEnvdBootstrapper struct {
	mu        sync.Mutex
	readyIPs  []string
	inits     []envd.InitOptions
	version   string
	readyErr  error
	initErr   error
	readyHook func(context.Context, string) error
}

func (f *fakeEnvdBootstrapper) WaitReady(ctx context.Context, ip string) error {
	f.mu.Lock()
	readyHook := f.readyHook
	f.readyIPs = append(f.readyIPs, ip)
	f.mu.Unlock()
	if readyHook != nil {
		return readyHook(ctx, ip)
	}
	return f.readyErr
}

func (f *fakeEnvdBootstrapper) Init(ctx context.Context, ip string, options envd.InitOptions) (string, error) {
	f.mu.Lock()
	f.inits = append(f.inits, options)
	version, initErr := f.version, f.initErr
	f.mu.Unlock()
	if initErr != nil {
		return "", initErr
	}
	return version, nil
}

// detachFixtureSlot drops the launch fixture's placeholder slot so a failure
// path's cleanup does not try to return it to the absent network pool.
func detachFixtureSlot(m *Manager, sandboxID string) {
	if value, ok := m.sandboxes.Load(sandboxID); ok {
		if sbx := value.(*sandboxEntry).sbx; sbx != nil {
			sbx.slot = nil
		}
	}
}

func e2bCreateRequest() CreateRequest {
	req := testCreateRequest()
	req.E2B = true
	req.Timeout = 5 * time.Minute
	req.TimeoutAction = runtimeapi.TimeoutActionPause
	req.MaskRequestHost = "app-${PORT}.internal"
	req.Metadata = map[string]string{"team": "core"}
	return req
}

func TestE2BCreateBootstrapsEnvdAndPublishesRoute(t *testing.T) {
	ctx := context.Background()
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	routes, capacity := newFakeRouteTable(), &fakeCapacityLedger{}
	envdClient := &fakeEnvdBootstrapper{version: "1.2.3"}
	m.Routes, m.Capacity, m.Envd = routes, capacity, envdClient
	req := e2bCreateRequest()
	req.Env = map[string]string{"FOO": "bar"}

	result, err := m.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if reservation, ok := capacity.holding("sandbox-a"); !ok || reservation != (capacityReservation{cpus: 2, memoryMB: 512}) {
		t.Fatalf("capacity reservation = %+v, %v", reservation, ok)
	}
	if capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("live sandbox lost its capacity reservation")
	}
	if len(routes.inserted) != 1 || routes.inserted[0] != "sandbox-a" {
		t.Fatalf("route inserts = %#v", routes.inserted)
	}
	if ip, ok := routes.routeIP("sandbox-a"); !ok || ip != result.IP {
		t.Fatalf("published route = %q, %v; result IP = %q", ip, ok, result.IP)
	}
	routes.mu.Lock()
	if routes.masks["sandbox-a"] != req.MaskRequestHost {
		t.Fatalf("published mask = %q", routes.masks["sandbox-a"])
	}
	routes.mu.Unlock()
	envdClient.mu.Lock()
	readyIPs, inits := append([]string(nil), envdClient.readyIPs...), append([]envd.InitOptions(nil), envdClient.inits...)
	envdClient.mu.Unlock()
	if len(readyIPs) != 1 || readyIPs[0] != result.IP {
		t.Fatalf("envd readiness probes = %#v", readyIPs)
	}
	if len(inits) != 1 || inits[0].EnvVars["FOO"] != "bar" ||
		inits[0].DefaultUser != "user" || inits[0].DefaultWorkdir != "/home/user" {
		t.Fatalf("envd init options = %#v", inits)
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.E2B || rec.State != StateReady || rec.EnvdVersion != "1.2.3" {
		t.Fatalf("record = %#v", rec)
	}
	if rec.StartedAt == 0 || rec.ExpiresAt <= rec.StartedAt {
		t.Fatalf("run timestamps = %d, %d", rec.StartedAt, rec.ExpiresAt)
	}
	if rec.TimeoutAction != req.TimeoutAction || rec.MaskRequestHost != req.MaskRequestHost || rec.Metadata["team"] != "core" {
		t.Fatalf("E2B fields = %#v", rec)
	}
	if result.EnvdVersion != "1.2.3" {
		t.Fatalf("result EnvdVersion = %q", result.EnvdVersion)
	}
	if value, ok := m.sandboxes.Load("sandbox-a"); !ok || value.(*sandboxEntry).state != StateReady {
		t.Fatalf("entry missing or not ready: %v", ok)
	}
}

func TestE2BCreateRequiresDependenciesAndTimeout(t *testing.T) {
	req := e2bCreateRequest()
	m, _, _ := newLifecycleTestManager(t, readyBoot())
	if _, err := m.Create(context.Background(), req); err == nil {
		t.Fatal("missing E2B dependencies accepted")
	}
	m.Routes, m.Capacity = newFakeRouteTable(), &fakeCapacityLedger{}
	m.Envd = &fakeEnvdBootstrapper{}
	req.Timeout = 0
	if _, err := m.Create(context.Background(), req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero timeout = %v", err)
	}
	req.Timeout = time.Minute
	m.Capacity = nil
	if _, err := m.Create(context.Background(), req); err == nil {
		t.Fatal("missing capacity ledger accepted")
	}
}

func TestE2BCreateFailsWhenRouteRegistryClosed(t *testing.T) {
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	routes := newFakeRouteTable()
	routes.closed = true
	capacity := &fakeCapacityLedger{}
	m.Routes, m.Capacity, m.Envd = routes, capacity, &fakeEnvdBootstrapper{}

	_, err := m.Create(context.Background(), e2bCreateRequest())
	if !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("closed registry = %v", err)
	}
	if _, ok := capacity.holding("sandbox-a"); ok || capacity.releaseCount("sandbox-a") != 1 {
		t.Fatal("closed registry leaked or kept the capacity reservation")
	}
	if _, err := store.Get(context.Background(), "sandbox-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record = %v", err)
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("entry remains")
	}
}

func TestE2BCreateEnvdFailureReleasesReservation(t *testing.T) {
	ctx := context.Background()
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	routes, capacity := newFakeRouteTable(), &fakeCapacityLedger{}
	envdDown := errors.New("envd down")
	envdClient := &fakeEnvdBootstrapper{readyErr: envdDown}
	m.Routes, m.Capacity, m.Envd = routes, capacity, envdClient
	envdClient.readyHook = func(context.Context, string) error {
		detachFixtureSlot(m, "sandbox-a")
		return envdDown
	}

	_, err := m.Create(ctx, e2bCreateRequest())
	if err == nil || !errors.Is(err, envdDown) {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := store.Get(ctx, "sandbox-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed fresh create left a record: %v", err)
	}
	if _, ok := capacity.holding("sandbox-a"); ok || capacity.releaseCount("sandbox-a") != 1 {
		t.Fatal("failed create kept or leaked the capacity reservation")
	}
	if routes.releaseCount("sandbox-a") != 1 {
		t.Fatalf("route releases = %d", routes.releaseCount("sandbox-a"))
	}
	if _, ok := m.sandboxes.Load("sandbox-a"); ok {
		t.Fatal("entry remains")
	}
}

func TestE2BCreateDuplicateRecordReleasesReservation(t *testing.T) {
	ctx := context.Background()
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	routes, capacity := newFakeRouteTable(), &fakeCapacityLedger{}
	m.Routes, m.Capacity, m.Envd = routes, capacity, &fakeEnvdBootstrapper{}
	req := e2bCreateRequest()
	foreign, err := store.Create(ctx, Record{
		ID: req.SandboxID, State: StatePaused, SourceTemplateID: req.TemplateID,
		CheckpointHeadTemplateID: digest.FromString("head").String(), E2B: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Create(ctx, req); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate create = %v", err)
	}
	kept, err := store.Get(ctx, req.SandboxID)
	if err != nil || kept.State != StatePaused || kept.CheckpointHeadTemplateID != foreign.CheckpointHeadTemplateID {
		t.Fatalf("foreign record = %#v, %v", kept, err)
	}
	if _, ok := capacity.holding(req.SandboxID); ok || capacity.releaseCount(req.SandboxID) != 1 {
		t.Fatal("duplicate create kept or leaked the capacity reservation")
	}
	if routes.releaseCount(req.SandboxID) != 1 {
		t.Fatalf("route releases = %d", routes.releaseCount(req.SandboxID))
	}
}

func TestE2BEnvdHandshakeFollowsRouteSlotLifetime(t *testing.T) {
	m, _, _ := newLifecycleTestManager(t, readyBoot())
	routes, capacity := newFakeRouteTable(), &fakeCapacityLedger{}
	envdClient := &fakeEnvdBootstrapper{}
	m.Routes, m.Capacity, m.Envd = routes, capacity, envdClient
	entered := make(chan struct{})
	envdClient.readyHook = func(ctx context.Context, ip string) error {
		close(entered)
		<-ctx.Done()
		detachFixtureSlot(m, "sandbox-a")
		return ctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		_, err := m.Create(context.Background(), e2bCreateRequest())
		done <- err
	}()
	<-entered
	routes.Release("sandbox-a")
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Create() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("envd handshake ignored the released route slot")
	}
}

func TestE2BCreateRetentionWaitsForExplicitDelete(t *testing.T) {
	ctx := context.Background()
	b := readyBoot()
	b.releaseErr = errors.New("boot release failed")
	m, store, _ := newLifecycleTestManager(t, b)
	routes, capacity := newFakeRouteTable(), &fakeCapacityLedger{}
	m.Routes, m.Capacity, m.Envd = routes, capacity, &fakeEnvdBootstrapper{}
	m.launch = func(context.Context, CreateRequest, VMStartSpec, createRuntimeIDs, bool) (*Sandbox, error) {
		return nil, errors.New("launch failed")
	}

	_, err := m.Create(ctx, e2bCreateRequest())
	if err == nil {
		t.Fatal("Create succeeded")
	}
	rec, err := store.Get(ctx, "sandbox-a")
	if err != nil || rec.State != StateUnknown {
		t.Fatalf("retained record = %#v, %v", rec, err)
	}
	if _, ok := capacity.holding("sandbox-a"); !ok || capacity.releaseCount("sandbox-a") != 0 {
		t.Fatal("unconfirmed cleanup must keep the capacity reservation")
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

func TestBootstrapAlive(t *testing.T) {
	if !bootstrapAlive(make(chan struct{})) {
		t.Fatal("open exit channel reported dead")
	}
	dead := make(chan struct{})
	close(dead)
	if bootstrapAlive(dead) {
		t.Fatal("closed exit channel reported alive")
	}
	if !bootstrapAlive(nil) {
		t.Fatal("nil exit channel reported dead")
	}
}

func pausedE2BRecord(req CreateRequest, head string) Record {
	return Record{
		ID: req.SandboxID, State: StateReady, SourceTemplateID: req.TemplateID,
		CheckpointHeadTemplateID: head, E2B: true, VCPUNum: req.VCPUNum, RamMB: req.RAMMB,
		StartedAt: 100, HeadStartedAt: 100, HeadCreatedAt: 200, ExpiresAt: 999, IP: "10.0.0.2", VMMPID: 4321,
		RuntimeSnapshots: []SnapshotRef{
			{Snapshotter: "erofs", Role: "rootfs", Key: req.SandboxID},
			{Snapshotter: "erofs", Role: "vm", Key: "view-vm-" + req.SandboxID},
		},
	}
}

// TestExitFinalizationKeepsE2BRecordReady pins the E2B crash rule: a crashed
// runtime leaves the record READY — externally the sandbox keeps running until
// its deadline — with its quota held. A confirmed cleanup clears the released
// run's view (IP, PID, snapshot refs) and drops the runtime entry; an
// unconfirmed cleanup keeps both so a delete or the expiry dispatch retries
// the release.
func TestExitFinalizationKeepsE2BRecordReady(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cleanupErr  error
		wantLastErr bool
	}{
		{"confirmed cleanup", nil, false},
		{"unconfirmed cleanup records the failure", errors.New("release network: failed"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b := readyBoot()
			b.releaseErr = tc.cleanupErr
			m, store, _ := newLifecycleTestManager(t, b)
			routes, capacity := newFakeRouteTable(), &fakeCapacityLedger{}
			m.Routes, m.Capacity = routes, capacity
			req := testCreateRequest()
			head := digest.FromString("head").String()
			if err := capacity.Reserve(req.SandboxID, req.VCPUNum, req.RAMMB); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(ctx, pausedE2BRecord(req, head)); err != nil {
				t.Fatal(err)
			}
			entry := &sandboxEntry{state: StateReady}
			m.sandboxes.Store(req.SandboxID, entry)

			m.handleSandboxExit(req.SandboxID, entry)
			rec, err := store.Get(ctx, req.SandboxID)
			if err != nil {
				t.Fatal(err)
			}
			if rec.State != StateReady {
				t.Fatalf("crashed record = %#v", rec)
			}
			if (rec.LastError != "") != tc.wantLastErr {
				t.Fatalf("crashed LastError = %q", rec.LastError)
			}
			// The deadline, the run start and the checkpoint head survive the crash.
			if rec.ExpiresAt != 999 || rec.StartedAt != 100 || rec.CheckpointHeadTemplateID != head {
				t.Fatalf("crashed lineage = %#v", rec)
			}
			// The quota stays with the still-READY record until its real deletion.
			if _, ok := capacity.holding(req.SandboxID); !ok || capacity.releaseCount(req.SandboxID) != 0 {
				t.Fatal("crashed sandbox lost its capacity reservation")
			}
			_, entryHeld := m.sandboxes.Load(req.SandboxID)
			if tc.cleanupErr == nil {
				// The released run's view is cleared and the entry dropped.
				if rec.IP != "" || rec.VMMPID != 0 || len(rec.RuntimeSnapshots) != 0 {
					t.Fatalf("crashed record = %#v", rec)
				}
				if entryHeld {
					t.Fatal("entry remains after finalization")
				}
			} else {
				// The unconfirmed release keeps the runtime view and the entry
				// so the cleanup can be retried through them.
				if rec.IP != "10.0.0.2" || rec.VMMPID != 4321 || len(rec.RuntimeSnapshots) != 2 {
					t.Fatalf("unconfirmed record = %#v", rec)
				}
				if !entryHeld {
					t.Fatal("entry dropped before the cleanup was confirmed")
				}
			}
		})
	}
}

// TestExitFinalizationSplitsNativeAndE2B pins the crash rule split: an E2B
// record — whatever its checkpoint lineage — stays READY holding its quota,
// while a native record becomes UNKNOWN with no quota to release.
func TestExitFinalizationSplitsNativeAndE2B(t *testing.T) {
	for _, tc := range []struct {
		name      string
		record    Record
		wantState State
		wantHeld  bool
		wantEntry bool
	}{
		{"e2b head equals source", pausedE2BRecord(testCreateRequest(), testCreateRequest().TemplateID), StateReady, true, false},
		{"native sandbox", func() Record {
			rec := pausedE2BRecord(testCreateRequest(), digest.FromString("head").String())
			rec.E2B = false
			return rec
		}(), StateUnknown, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, store, _ := newLifecycleTestManager(t, readyBoot())
			capacity := &fakeCapacityLedger{}
			m.Capacity = capacity
			if tc.record.E2B {
				if err := capacity.Reserve(tc.record.ID, tc.record.VCPUNum, tc.record.RamMB); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.Create(ctx, tc.record); err != nil {
				t.Fatal(err)
			}
			entry := &sandboxEntry{state: StateReady}
			m.sandboxes.Store(tc.record.ID, entry)

			m.handleSandboxExit(tc.record.ID, entry)
			rec, err := store.Get(ctx, tc.record.ID)
			if err != nil || rec.State != tc.wantState {
				t.Fatalf("record = %#v, %v", rec, err)
			}
			if _, ok := capacity.holding(tc.record.ID); ok != tc.wantHeld {
				t.Fatalf("capacity held = %v, want %v", ok, tc.wantHeld)
			}
			if got := capacity.releaseCount(tc.record.ID); got != 0 {
				t.Fatalf("capacity releases = %d, want 0", got)
			}
			if _, ok := m.sandboxes.Load(tc.record.ID); ok != tc.wantEntry {
				t.Fatalf("entry present = %v, want %v", ok, tc.wantEntry)
			}
		})
	}
}

// A record read failure inside finalizeDeath leaves everything in place: the
// quota stays with the still-READY record and the entry stays UNKNOWN, so the
// deadline dispatch recognizes the dead runtime and retries the cleanup.
func TestFinalizeDeathReadFailureKeepsQuota(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cleanupErr error
	}{
		{"confirmed cleanup", nil},
		{"unconfirmed cleanup", errors.New("release network: failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, store, _ := newLifecycleTestManager(t, readyBoot())
			capacity := &fakeCapacityLedger{}
			m.Routes, m.Capacity = newFakeRouteTable(), capacity
			req := testCreateRequest()
			if err := capacity.Reserve(req.SandboxID, req.VCPUNum, req.RAMMB); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(ctx, pausedE2BRecord(req, digest.FromString("head").String())); err != nil {
				t.Fatal(err)
			}
			entry := &sandboxEntry{state: StateReady}
			m.sandboxes.Store(req.SandboxID, entry)
			store.getHook = func(string) error { return errors.New("bolt unavailable") }

			m.finalizeDeath(ctx, req.SandboxID, entry, tc.cleanupErr)

			if _, held := capacity.holding(req.SandboxID); !held || capacity.releaseCount(req.SandboxID) != 0 {
				t.Fatal("read failure released the capacity reservation")
			}
			store.getHook = nil
			rec, err := store.Get(ctx, req.SandboxID)
			if err != nil || rec.State != StateReady || rec.LastError != "" {
				t.Fatalf("record = %#v (%v)", rec, err)
			}
			value, ok := m.sandboxes.Load(req.SandboxID)
			if !ok {
				t.Fatal("entry was dropped before the deadline retry")
			}
			if current := value.(*sandboxEntry); current != entry || current.state != StateUnknown {
				t.Fatalf("entry = %#v", current)
			}
		})
	}
}

// TestFinalizeDeathReleasesCreateLease pins the natural-exit lease release:
// every exit branch — native, E2B, unconfirmed cleanup, failed record read —
// drops the deterministic create lease, and a failed release only logs
// without changing the crash rule's outcome.
func TestFinalizeDeathReleasesCreateLease(t *testing.T) {
	for _, tc := range []struct {
		name       string
		native     bool
		cleanupErr error
		readFails  bool
		leaseErr   error
		wantLease  bool
		wantState  State
		wantEntry  bool
	}{
		{"native record", true, nil, false, nil, false, StateUnknown, true},
		{"e2b record", false, nil, false, nil, false, StateReady, false},
		{"unconfirmed cleanup", false, errors.New("release network: failed"), false, nil, false, StateReady, true},
		{"record read failure", false, nil, true, nil, false, StateReady, true},
		{"failed release only logs", false, nil, false, errors.New("lease delete failed"), true, StateReady, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, store, leaseStore := newLifecycleTestManager(t, readyBoot())
			m.Routes, m.Capacity = newFakeRouteTable(), &fakeCapacityLedger{}
			rec := pausedE2BRecord(testCreateRequest(), digest.FromString("head").String())
			rec.E2B = !tc.native
			if _, err := store.Create(ctx, rec); err != nil {
				t.Fatal(err)
			}
			leaseID := "sandbox-create-" + rec.ID
			leaseStore.items[leaseID] = leases.Lease{ID: leaseID}
			leaseStore.deleteErr = tc.leaseErr
			entry := &sandboxEntry{state: StateReady}
			m.sandboxes.Store(rec.ID, entry)
			if tc.readFails {
				store.getHook = func(string) error { return errors.New("bolt unavailable") }
			}

			m.finalizeDeath(ctx, rec.ID, entry, tc.cleanupErr)

			if _, held := leaseStore.items[leaseID]; held != tc.wantLease {
				t.Fatalf("create lease held = %v, want %v", held, tc.wantLease)
			}
			store.getHook = nil
			after, err := store.Get(ctx, rec.ID)
			if err != nil || after.State != tc.wantState {
				t.Fatalf("record = %#v (%v)", after, err)
			}
			if _, held := m.sandboxes.Load(rec.ID); held != tc.wantEntry {
				t.Fatalf("entry present = %v, want %v", held, tc.wantEntry)
			}
		})
	}
}

func TestDeleteReleasesE2BCapacityRegardlessOfLineage(t *testing.T) {
	ctx := context.Background()
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	routes, capacity := newFakeRouteTable(), &fakeCapacityLedger{}
	m.Routes, m.Capacity = routes, capacity
	req := testCreateRequest()
	if _, err := store.Create(ctx, pausedE2BRecord(req, digest.FromString("head").String())); err != nil {
		t.Fatal(err)
	}
	m.sandboxes.Store(req.SandboxID, &sandboxEntry{state: StateReady})

	if err := m.Delete(ctx, req.SandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, req.SandboxID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("explicit delete kept the record: %v", err)
	}
	if _, ok := capacity.holding(req.SandboxID); ok || capacity.releaseCount(req.SandboxID) != 1 {
		t.Fatal("explicit delete kept or leaked the capacity reservation")
	}
	if routes.releaseCount(req.SandboxID) != 1 {
		t.Fatalf("route releases = %d", routes.releaseCount(req.SandboxID))
	}
}

// A failed terminal write must not let the terminal lineage rule
// resurrect a sandbox the user explicitly deleted.
func TestDeleteFailureKeepsDeletionIntent(t *testing.T) {
	ctx := context.Background()
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	capacity := &fakeCapacityLedger{}
	m.Routes, m.Capacity = newFakeRouteTable(), capacity
	req := testCreateRequest()
	if _, err := store.Create(ctx, pausedE2BRecord(req, digest.FromString("head").String())); err != nil {
		t.Fatal(err)
	}
	m.sandboxes.Store(req.SandboxID, &sandboxEntry{state: StateReady})
	if err := capacity.Reserve(req.SandboxID, req.VCPUNum, req.RAMMB); err != nil {
		t.Fatal(err)
	}
	store.deleteHook = func() error { return errors.New("store down") }

	if err := m.Delete(ctx, req.SandboxID); err == nil {
		t.Fatal("terminal write failure was swallowed")
	}
	rec, err := store.Get(ctx, req.SandboxID)
	if err != nil || rec.State != StateUnknown {
		t.Fatalf("failed delete record = %#v, %v", rec, err)
	}
	if rec.CheckpointHeadTemplateID != rec.SourceTemplateID {
		t.Fatalf("failed delete kept the recoverable lineage: %#v", rec)
	}
	if _, ok := capacity.holding(req.SandboxID); !ok {
		t.Fatal("retained record lost the capacity reservation")
	}

	store.deleteHook = nil
	if err := m.Delete(ctx, req.SandboxID); err != nil {
		t.Fatalf("Delete() retry error = %v", err)
	}
	if _, err := store.Get(ctx, req.SandboxID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retried delete resurrected the record: %v", err)
	}
	if _, ok := capacity.holding(req.SandboxID); ok || capacity.releaseCount(req.SandboxID) != 1 {
		t.Fatal("retried delete kept or leaked the capacity reservation")
	}
	if _, ok := m.sandboxes.Load(req.SandboxID); ok {
		t.Fatal("entry remains after the delete retry")
	}
}

// TestFinalizeReleasedWriteBackFailureStrandsRecord pins the terminal-failure
// rule for the PAUSED write: the release is confirmed, so the entry and the
// capacity reservation go, while the record goes UNKNOWN with its lineage
// kept for a later resume or delete.
func TestFinalizeReleasedWriteBackFailureStrandsRecord(t *testing.T) {
	ctx := context.Background()
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	capacity := &fakeCapacityLedger{}
	m.Routes, m.Capacity = newFakeRouteTable(), capacity
	req := testCreateRequest()
	head := digest.FromString("head").String()
	if err := capacity.Reserve(req.SandboxID, req.VCPUNum, req.RAMMB); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Create(ctx, pausedE2BRecord(req, head))
	if err != nil {
		t.Fatal(err)
	}
	m.sandboxes.Store(req.SandboxID, &sandboxEntry{state: StateReady})
	writeErr := errors.New("paused write failure")
	store.updateHook = func(r Record) error {
		if r.State == StatePaused {
			return writeErr
		}
		return nil
	}

	if err := m.finalizeReleased(ctx, rec, finalizationOptions{}); !errors.Is(err, writeErr) {
		t.Fatalf("finalizeReleased() error = %v", err)
	}
	got, err := store.Get(ctx, req.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateUnknown || got.LastError != writeErr.Error() {
		t.Fatalf("stranded record = %#v", got)
	}
	if got.CheckpointHeadTemplateID != head {
		t.Fatalf("stranded record lost its lineage: %#v", got)
	}
	if got.IP != "" || got.VMMPID != 0 || got.ExpiresAt != 0 || len(got.RuntimeSnapshots) != 0 {
		t.Fatalf("stranded record kept the released run's view: %#v", got)
	}
	if _, ok := m.sandboxes.Load(req.SandboxID); ok {
		t.Fatal("entry remains after the confirmed release")
	}
	if _, ok := capacity.holding(req.SandboxID); ok || capacity.releaseCount(req.SandboxID) != 1 {
		t.Fatal("confirmed release kept or leaked the capacity reservation")
	}
}
