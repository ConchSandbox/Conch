package netstack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	cni "github.com/containerd/go-cni"
	"github.com/openeuler/Conch/internal/daemon/state"
	slotstate "github.com/openeuler/Conch/internal/netstack/slot"
)

func TestNormalizeAndValidateWarmPoolSize(t *testing.T) {
	tests := []struct {
		name       string
		warm       int
		wantWarm   int
		wantErrSub string
	}{
		{name: "defaults", wantWarm: DefaultWarmPoolSize},
		{name: "explicit", warm: 100, wantWarm: 100},
		{name: "maximum", warm: maxSlots, wantWarm: maxSlots},
		{name: "exceeds maximum", warm: maxSlots + 1, wantErrSub: "maximum supported slots"},
		{name: "negative", warm: -1, wantErrSub: "warm_pool_size"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotWarm, err := normalizeAndValidateWarmPoolSize(tt.warm)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("normalizeAndValidateWarmPoolSize() error = %v, want containing %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeAndValidateWarmPoolSize() error = %v", err)
			}
			if gotWarm != tt.wantWarm {
				t.Fatalf("normalizeAndValidateWarmPoolSize() = %d, want %d", gotWarm, tt.wantWarm)
			}
		})
	}
}

func TestDynamicPopulateBacksOffAfterAllocationFailure(t *testing.T) {
	store := newFakeNetworkSlotStore()
	store.createAttempts = make(chan struct{}, 2)
	store.createErr = errors.New("state store unavailable")
	p := &Pool{
		warmSlots:          slotstate.NewQueue[*Slot](1),
		dynamicReservation: true,
		prefillReady:       make(chan struct{}),
		slotStore:          store,
		slotIDs:            slotstate.NewAllocator(firstSlotID, maxSlots),
	}
	p.Start(context.Background())

	select {
	case <-store.createAttempts:
	case <-time.After(time.Second):
		t.Fatal("Populate() did not attempt slot allocation")
	}
	select {
	case <-store.createAttempts:
		t.Fatal("Populate() retried without backoff")
	case <-time.After(100 * time.Millisecond):
	}

	closeDone := make(chan struct{})
	go func() {
		p.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close() did not stop the populate loop")
	}
	p.Close()
}

type fakeNetworkSlotStore struct {
	mu             sync.Mutex
	records        map[int]state.NetworkSlotRecord
	updates        []state.NetworkSlotRecord
	deletes        []int
	listErr        error
	getErr         error
	createErr      error
	updateErr      error
	deleteErr      error
	createAttempts chan struct{}
}

type fakeCNIPlugin struct {
	setup  func(context.Context, string, string, ...cni.NamespaceOpts) (*cni.Result, error)
	remove func(context.Context, string, string, ...cni.NamespaceOpts) error
}

func (f *fakeCNIPlugin) Setup(ctx context.Context, id, path string, opts ...cni.NamespaceOpts) (*cni.Result, error) {
	if f.setup == nil {
		return nil, nil
	}
	return f.setup(ctx, id, path, opts...)
}

func (f *fakeCNIPlugin) SetupSerially(ctx context.Context, id, path string, opts ...cni.NamespaceOpts) (*cni.Result, error) {
	return f.Setup(ctx, id, path, opts...)
}

func (f *fakeCNIPlugin) Remove(ctx context.Context, id, path string, opts ...cni.NamespaceOpts) error {
	if f.remove == nil {
		return nil
	}
	return f.remove(ctx, id, path, opts...)
}

func (*fakeCNIPlugin) Check(context.Context, string, string, ...cni.NamespaceOpts) error {
	return nil
}

func (*fakeCNIPlugin) Load(...cni.Opt) error { return nil }

func (*fakeCNIPlugin) Status() error { return nil }

func (*fakeCNIPlugin) GetConfig() *cni.ConfigResult { return nil }

func newFakeNetworkSlotStore(records ...state.NetworkSlotRecord) *fakeNetworkSlotStore {
	store := &fakeNetworkSlotStore{records: make(map[int]state.NetworkSlotRecord)}
	for _, rec := range records {
		store.records[rec.SlotID] = rec
	}
	return store
}

func testSlotIDAllocator(t *testing.T, usedIDs ...int) *slotstate.Allocator {
	t.Helper()
	allocator := slotstate.NewAllocator(firstSlotID, maxSlots)
	if err := allocator.Rebuild(usedIDs); err != nil {
		t.Fatalf("rebuild slot ID allocator: %v", err)
	}
	return allocator
}

func (s *fakeNetworkSlotStore) CreateNetworkSlot(ctx context.Context, rec state.NetworkSlotRecord) error {
	if s.createAttempts != nil {
		select {
		case s.createAttempts <- struct{}{}:
		default:
		}
	}
	if s.createErr != nil {
		return s.createErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = make(map[int]state.NetworkSlotRecord)
	}
	if _, ok := s.records[rec.SlotID]; ok {
		return state.ErrAlreadyExists
	}
	s.records[rec.SlotID] = rec
	return nil
}

func (s *fakeNetworkSlotStore) UpdateNetworkSlot(ctx context.Context, rec state.NetworkSlotRecord) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[rec.SlotID]; !ok {
		return state.ErrNotFound
	}
	s.records[rec.SlotID] = rec
	s.updates = append(s.updates, rec)
	return nil
}

func (s *fakeNetworkSlotStore) GetNetworkSlot(ctx context.Context, id int) (state.NetworkSlotRecord, error) {
	if s.getErr != nil {
		return state.NetworkSlotRecord{}, s.getErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return state.NetworkSlotRecord{}, state.ErrNotFound
	}
	return rec, nil
}

func (s *fakeNetworkSlotStore) ListNetworkSlots(ctx context.Context) ([]state.NetworkSlotRecord, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]state.NetworkSlotRecord, 0, len(s.records))
	for _, rec := range s.records {
		records = append(records, rec)
	}
	return records, nil
}

func (s *fakeNetworkSlotStore) DeleteNetworkSlot(ctx context.Context, id int) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, id)
	s.deletes = append(s.deletes, id)
	return nil
}

func TestIsExpectedShutdownError(t *testing.T) {
	activeCtx := context.Background()
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	preserveCtx, preserveCancel := context.WithCancel(withPreserveOnCancel(context.Background()))
	preserveCancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "nil", ctx: canceledCtx, err: nil, want: false},
		{name: "active context", ctx: activeCtx, err: context.Canceled, want: false},
		{name: "plain canceled", ctx: canceledCtx, err: context.Canceled, want: false},
		{name: "plain interrupt text", ctx: canceledCtx, err: fmt.Errorf("command failed: signal: interrupt"), want: false},
		{name: "preserve typed canceled", ctx: preserveCtx, err: context.Canceled, want: true},
		{name: "preserve typed deadline", ctx: preserveCtx, err: context.DeadlineExceeded, want: true},
		{name: "preserve interrupt text", ctx: preserveCtx, err: fmt.Errorf("command failed: signal: interrupt"), want: true},
		{name: "non shutdown", ctx: canceledCtx, err: errors.New("permission denied"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExpectedShutdownError(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("isExpectedShutdownError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateSlotRecordPersistsCNIResult(t *testing.T) {
	store := newFakeNetworkSlotStore()
	p := &Pool{slotStore: store}
	slot, err := NewSlot(firstSlotID)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	store.records[slot.ID] = state.NetworkSlotRecord{SlotID: slot.ID, State: state.NetworkSlotTransient}

	if err := p.updateSlotRecord(context.Background(), slot, state.NetworkSlotTransient, "", nil); err != nil {
		t.Fatalf("updateSlotRecord() before cni result: %v", err)
	}
	rec := store.records[slot.ID]
	if rec.CNIIP != "" {
		t.Fatalf("record before cni result has CNIIP=%q, want empty", rec.CNIIP)
	}

	slot.setCNIResult(&CNIResult{IP: "10.12.0.2"})
	if err := p.updateSlotRecord(context.Background(), slot, state.NetworkSlotTransient, "", nil); err != nil {
		t.Fatalf("updateSlotRecord() after cni result: %v", err)
	}
	rec = store.records[slot.ID]
	if rec.CNIIP != "10.12.0.2" {
		t.Fatalf("record after cni result has CNIIP=%q, want 10.12.0.2", rec.CNIIP)
	}
}

func TestSetupSlotNetworkDoesNotPersistBeforeAdd(t *testing.T) {
	store := newFakeNetworkSlotStore()
	slot, err := NewSlot(firstSlotID)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	store.records[slot.ID] = state.NetworkSlotRecord{
		SlotID: slot.ID,
		State:  state.NetworkSlotTransient,
	}

	setupErr := errors.New("cni add failed")
	removeCalls := 0
	plugin := &fakeCNIPlugin{
		setup: func(ctx context.Context, _ string, _ string, _ ...cni.NamespaceOpts) (*cni.Result, error) {
			rec, getErr := store.GetNetworkSlot(ctx, slot.ID)
			if getErr != nil {
				t.Fatalf("GetNetworkSlot() during ADD: %v", getErr)
			}
			if rec.CNIIP != "" {
				t.Fatalf("record during ADD has CNIIP=%q, want empty", rec.CNIIP)
			}
			if len(store.updates) != 0 {
				t.Fatalf("record was updated %d times before ADD", len(store.updates))
			}
			return nil, setupErr
		},
		remove: func(context.Context, string, string, ...cni.NamespaceOpts) error {
			removeCalls++
			return nil
		},
	}
	p := &Pool{
		slotStore: store,
		cniManager: &CNIManager{
			plugin: plugin,
			config: CNIManagerConfig{IfName: defaultCNIIfName},
		},
	}

	err = p.setupSlotNetwork(context.Background(), slot)
	if !errors.Is(err, setupErr) {
		t.Fatalf("setupSlotNetwork() error = %v, want %v", err, setupErr)
	}
	if removeCalls != 0 {
		t.Fatalf("CNI Remove calls during setup = %d, want 0; Pool cleanup owns DEL", removeCalls)
	}
	if slot.CNIResult() != nil {
		t.Fatalf("slot CNI result after failed ADD = %#v, want nil", slot.CNIResult())
	}
}

func TestTeardownDerivesCNIIdentityWithoutPersistedIntent(t *testing.T) {
	slot, err := NewSlot(firstSlotID)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}

	removeCalls := 0
	p := &Pool{cniManager: &CNIManager{plugin: &fakeCNIPlugin{
		remove: func(_ context.Context, id, path string, _ ...cni.NamespaceOpts) error {
			removeCalls++
			if id != slot.CNIContainerID() || path != "" {
				t.Fatalf("Remove(%q, %q), want (%q, empty)", id, path, slot.CNIContainerID())
			}
			return nil
		},
	}}}
	if err := p.teardownSandboxNetworkWithRetry(context.Background(), slot, t.TempDir()+"/missing"); err != nil {
		t.Fatalf("teardownSandboxNetworkWithRetry(): %v", err)
	}
	if removeCalls != 1 {
		t.Fatalf("CNI Remove calls = %d, want 1", removeCalls)
	}
}

func TestHandleCreatedSlotAfterPreserveCancelRecordsIdle(t *testing.T) {
	store := newFakeNetworkSlotStore()
	p := &Pool{
		slotStore: store,
	}
	slot, err := NewSlot(firstSlotID)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	store.records[slot.ID] = state.NetworkSlotRecord{SlotID: slot.ID, State: state.NetworkSlotTransient}

	ctx, cancel := context.WithCancel(withPreserveOnCancel(context.Background()))
	cancel()
	p.handleCreatedSlotAfterCancel(ctx, slot)

	rec, ok := store.records[slot.ID]
	if !ok {
		t.Fatalf("slot record missing")
	}
	if rec.State != state.NetworkSlotIdle {
		t.Fatalf("slot record state = %q, want %q", rec.State, state.NetworkSlotIdle)
	}
}

func TestHandleCreatedSlotAfterPlainCancelDiscardsSlot(t *testing.T) {
	store := newFakeNetworkSlotStore()
	slot, err := NewSlot(firstSlotID)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	store.records[slot.ID] = state.NetworkSlotRecord{SlotID: slot.ID, State: state.NetworkSlotTransient}
	p := &Pool{
		slotStore:  store,
		slotIDs:    testSlotIDAllocator(t, slot.ID),
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.handleCreatedSlotAfterCancel(ctx, slot)

	if _, ok := store.records[slot.ID]; ok {
		t.Fatalf("slot record still exists after discard")
	}
	reused, err := p.slotIDs.Acquire()
	if err != nil || reused != slot.ID {
		t.Fatalf("acquire after discard = (%d, %v), want (%d, nil)", reused, err, slot.ID)
	}
}

func TestCNIBusyErrorDetectionAndChainParsing(t *testing.T) {
	if !isCNIBusyTeardownError(errors.New("CHAIN_DEL failed: Device or resource busy")) {
		t.Fatalf("isCNIBusyTeardownError() = false, want true")
	}
	if isCNIBusyTeardownError(errors.New("some other cni error")) {
		t.Fatalf("isCNIBusyTeardownError(non-busy) = true, want false")
	}

	tests := map[string]string{
		`iptables: chain CNI-1234567890abcdef is busy`:             "CNI-1234567890abcdef",
		`running ["iptables" "-X" "CNI-abc123", "--wait"]`:         "CNI-abc123",
		`CHAIN_DEL failed (Device or resource busy): CNI-deadbeef`: "CNI-deadbeef",
		`no chain here`: "",
	}
	for msg, want := range tests {
		if got := cniBusyChain(errors.New(msg)); got != want {
			t.Fatalf("cniBusyChain(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestPoolCloseRejectsGetWithBufferedSlots(t *testing.T) {
	p := &Pool{
		warmSlots:    slotstate.NewQueue[*Slot](1),
		prefillReady: make(chan struct{}),
	}
	if err := p.warmSlots.Push(&Slot{ID: firstSlotID}); err != nil {
		t.Fatalf("push buffered slot: %v", err)
	}

	p.Close()

	if _, err := p.Get(context.Background(), "sandbox-a"); !errors.Is(err, errWarmPoolClosed) {
		t.Fatalf("Get() after Close() error = %v, want errWarmPoolClosed", err)
	}
	size, _ := p.warmSlots.Usage()
	closed := p.warmSlots.IsClosed()
	if size != 1 || !closed {
		t.Fatalf("warm queue after Close() = (size=%d, closed=%v), want buffered slot preserved in closed queue", size, closed)
	}
}

func TestReplenishDroppedSlotSkipsClosedPool(t *testing.T) {
	store := newFakeNetworkSlotStore()
	store.createAttempts = make(chan struct{}, 1)
	p := &Pool{
		warmSlots: slotstate.NewQueue[*Slot](1),
		slotStore: store,
	}
	p.warmSlots.Close()

	if err := p.replenishDroppedSlot(context.Background()); err != nil {
		t.Fatalf("replenishDroppedSlot() after close: %v", err)
	}
	select {
	case <-store.createAttempts:
		t.Fatal("CreateNetworkSlot called for a closed pool")
	default:
	}
}

func TestRestoreAssignedRejectsMissingNamespace(t *testing.T) {
	store := newFakeNetworkSlotStore(state.NetworkSlotRecord{
		SlotID:    firstSlotID,
		State:     state.NetworkSlotAssigned,
		SandboxID: "sandbox-a",
		CNIIP:     "10.12.0.2",
	})
	p := &Pool{
		cniManager: &CNIManager{config: CNIManagerConfig{IfName: defaultCNIIfName}},
		slotStore:  store,
	}
	slot, err := NewSlot(firstSlotID)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	err = p.RestoreAssigned(slot, "sandbox-a", "10.12.0.2")
	if err == nil || !strings.Contains(err.Error(), "namespace missing") {
		t.Fatalf("RestoreAssigned() error = %v, want namespace missing", err)
	}
	if slot.CNIResult() != nil {
		t.Fatalf("CNIResult() = %#v, want nil after failed restore", slot.CNIResult())
	}
	rec := store.records[slot.ID]
	if rec.State != state.NetworkSlotAssigned {
		t.Fatalf("slot record state = %q, want %q until recovery cleanup runs", rec.State, state.NetworkSlotAssigned)
	}
	if rec.SandboxID != "sandbox-a" {
		t.Fatalf("slot record SandboxID = %q, want sandbox-a", rec.SandboxID)
	}
	if rec.LastError != "" {
		t.Fatalf("slot record LastError = %q, want unchanged record", rec.LastError)
	}
}

func TestAdoptIdleCleansTransientRecords(t *testing.T) {
	store := newFakeNetworkSlotStore(
		state.NetworkSlotRecord{SlotID: firstSlotID, State: state.NetworkSlotTransient},
		state.NetworkSlotRecord{SlotID: firstSlotID + 1, State: state.NetworkSlotTransient},
	)
	removeCalls := 0
	p := &Pool{
		slotStore: store,
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{
			remove: func(context.Context, string, string, ...cni.NamespaceOpts) error {
				removeCalls++
				return nil
			},
		}},
		warmSlots: slotstate.NewQueue[*Slot](2),
	}

	adopted, err := p.AdoptIdle(context.Background())
	if err != nil {
		t.Fatalf("AdoptIdle() error = %v", err)
	}
	if adopted != 0 {
		t.Fatalf("AdoptIdle() adopted = %d, want 0", adopted)
	}
	if len(store.records) != 0 {
		t.Fatalf("remaining slot records = %#v, want none", store.records)
	}
	if removeCalls != 2 {
		t.Fatalf("CNI Remove calls = %d, want 2", removeCalls)
	}
	for _, want := range []int{firstSlotID, firstSlotID + 1} {
		got, acquireErr := p.slotIDs.Acquire()
		if acquireErr != nil || got != want {
			t.Fatalf("acquire cleaned slot ID = (%d, %v), want (%d, nil)", got, acquireErr, want)
		}
	}
}

func TestAdoptIdleCleansInvalidIdleRecord(t *testing.T) {
	store := newFakeNetworkSlotStore(
		state.NetworkSlotRecord{SlotID: firstSlotID, State: state.NetworkSlotIdle},
	)
	p := &Pool{
		slotStore:  store,
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{}, config: CNIManagerConfig{IfName: defaultCNIIfName}},
		warmSlots:  slotstate.NewQueue[*Slot](1),
	}

	adopted, err := p.AdoptIdle(context.Background())
	if err != nil {
		t.Fatalf("AdoptIdle() error = %v", err)
	}
	if adopted != 0 {
		t.Fatalf("AdoptIdle() adopted = %d, want 0", adopted)
	}
	if len(store.records) != 0 {
		t.Fatalf("remaining slot records = %#v, want none", store.records)
	}
}

func TestGetRequeuesSlotWhenAssignmentRecordFails(t *testing.T) {
	slot, err := NewSlot(firstSlotID)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	storeErr := errors.New("store unavailable")
	p := &Pool{
		slotStore: storeErrNetworkSlotStore(storeErr),
		warmSlots: slotstate.NewQueue[*Slot](1),
	}
	if err := p.warmSlots.Push(slot); err != nil {
		t.Fatalf("push slot: %v", err)
	}

	got, err := p.Get(context.Background(), "sandbox-a")
	if err == nil || !strings.Contains(err.Error(), "failed to record assigned network slot") {
		t.Fatalf("Get() error = %v, want assignment record failure", err)
	}
	if got != nil {
		t.Fatalf("Get() slot = %#v, want nil", got)
	}
	if slot.SandboxID() != "" {
		t.Fatalf("slot SandboxID = %q, want cleared", slot.SandboxID())
	}
	requeued, popErr := p.warmSlots.Pop()
	if popErr != nil {
		t.Fatalf("slot was not requeued after assignment record failure: %v", popErr)
	}
	if requeued != slot {
		t.Fatalf("requeued slot = %#v, want original slot", requeued)
	}
}

func TestCleanupAssignedWithoutReadySandbox(t *testing.T) {
	store := newFakeNetworkSlotStore(
		state.NetworkSlotRecord{SlotID: firstSlotID, State: state.NetworkSlotAssigned, SandboxID: "sandbox-gone"},
		state.NetworkSlotRecord{SlotID: firstSlotID + 1, State: state.NetworkSlotAssigned, SandboxID: "sandbox-ready"},
	)
	p := &Pool{
		slotStore:  store,
		slotIDs:    testSlotIDAllocator(t, firstSlotID, firstSlotID+1),
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{}},
	}

	err := p.CleanupAssignedWithoutReadySandbox(map[string]struct{}{"sandbox-ready": {}})
	if err != nil {
		t.Fatalf("CleanupAssignedWithoutReadySandbox() error = %v", err)
	}
	if _, ok := store.records[firstSlotID]; ok {
		t.Fatalf("stale assigned slot record still exists")
	}
	if rec, ok := store.records[firstSlotID+1]; !ok || rec.SandboxID != "sandbox-ready" {
		t.Fatalf("ready assigned slot record = %#v, ok=%v; want preserved", rec, ok)
	}
	reused, acquireErr := p.slotIDs.Acquire()
	if acquireErr != nil || reused != firstSlotID {
		t.Fatalf("acquire cleaned assigned slot ID = (%d, %v), want (%d, nil)", reused, acquireErr, firstSlotID)
	}
}

func storeErrNetworkSlotStore(err error) *fakeNetworkSlotStore {
	return &fakeNetworkSlotStore{records: make(map[int]state.NetworkSlotRecord), updateErr: err}
}
