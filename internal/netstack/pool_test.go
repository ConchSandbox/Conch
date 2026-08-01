package netstack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/daemon/state"
)

type fakeStorage struct {
	released int
	acquired int
}

func (s *fakeStorage) Acquire(ctx context.Context) (*Slot, error) {
	s.acquired++
	return nil, errors.New("unexpected acquire")
}

func (s *fakeStorage) Release(slot *Slot) error {
	s.released++
	return nil
}

type fakeNetworkSlotStore struct {
	records   map[string]state.NetworkSlotRecord
	upserts   []state.NetworkSlotRecord
	deletes   []string
	listErr   error
	upsertErr error
}

func newFakeNetworkSlotStore(records ...state.NetworkSlotRecord) *fakeNetworkSlotStore {
	store := &fakeNetworkSlotStore{records: make(map[string]state.NetworkSlotRecord)}
	for _, rec := range records {
		store.records[rec.SlotKey] = rec
	}
	return store
}

func (s *fakeNetworkSlotStore) UpsertNetworkSlot(ctx context.Context, rec state.NetworkSlotRecord) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	if s.records == nil {
		s.records = make(map[string]state.NetworkSlotRecord)
	}
	s.records[rec.SlotKey] = rec
	s.upserts = append(s.upserts, rec)
	return nil
}

func (s *fakeNetworkSlotStore) ListNetworkSlots(ctx context.Context) ([]state.NetworkSlotRecord, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	records := make([]state.NetworkSlotRecord, 0, len(s.records))
	for _, rec := range s.records {
		records = append(records, rec)
	}
	return records, nil
}

func (s *fakeNetworkSlotStore) DeleteNetworkSlot(ctx context.Context, key string) error {
	delete(s.records, key)
	s.deletes = append(s.deletes, key)
	return nil
}

func TestIsExpectedShutdownError(t *testing.T) {
	activeCtx := context.Background()
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	preserveCtx, preserveCancel := context.WithCancel(WithPreserveOnCancel(context.Background()))
	preserveCancel()
	cleanupCtx, cleanupCancel := context.WithCancelCause(WithPreserveOnCancel(context.Background()))
	cleanupCancel(errPoolCleanupRequested)

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
		{name: "explicit cleanup cancel", ctx: cleanupCtx, err: context.Canceled, want: false},
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

func TestUpsertSlotRecordPersistsCNIIDOnlyWithResult(t *testing.T) {
	store := newFakeNetworkSlotStore()
	p := &Pool{slotStore: store}
	slot, err := NewSlot("2", firstSlotIndex)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}

	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCreating, "", nil); err != nil {
		t.Fatalf("upsertSlotRecord() before cni result: %v", err)
	}
	rec := store.records[slot.Key]
	if rec.CNIID != "" || rec.CNIIP != "" {
		t.Fatalf("record before cni result has CNIID=%q CNIIP=%q, want empty", rec.CNIID, rec.CNIIP)
	}

	slot.setSlotNetwork(slot.CNIContainerID(), &CNIResult{IP: "10.12.0.2"}, nil)
	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCreating, "", nil); err != nil {
		t.Fatalf("upsertSlotRecord() after cni result: %v", err)
	}
	rec = store.records[slot.Key]
	if rec.CNIID != slot.CNIContainerID() || rec.CNIIP != "10.12.0.2" {
		t.Fatalf("record after cni result has CNIID=%q CNIIP=%q, want %q/10.12.0.2", rec.CNIID, rec.CNIIP, slot.CNIContainerID())
	}
}

func TestHandleCreatedSlotAfterPreserveCancelRecordsWarmIdle(t *testing.T) {
	store := newFakeNetworkSlotStore()
	p := &Pool{
		slotStore: store,
	}
	slot, err := NewSlot("2", firstSlotIndex)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	slot.setNetNSPath(t.TempDir() + "/ns-2")

	ctx, cancel := context.WithCancel(WithPreserveOnCancel(context.Background()))
	cancel()
	p.handleCreatedSlotAfterCancel(ctx, slot)

	rec, ok := store.records[slot.Key]
	if !ok {
		t.Fatalf("slot record missing")
	}
	if rec.State != state.NetworkSlotWarmIdle {
		t.Fatalf("slot record state = %q, want %q", rec.State, state.NetworkSlotWarmIdle)
	}
	if rec.NetNSPath != slot.NetNSPath() {
		t.Fatalf("slot record NetNSPath = %q, want %q", rec.NetNSPath, slot.NetNSPath())
	}
}

func TestHandleCreatedSlotAfterPlainCancelDiscardsSlot(t *testing.T) {
	store := newFakeNetworkSlotStore()
	storage := &fakeStorage{}
	p := &Pool{
		slotStorage: storage,
		slotStore:   store,
	}
	slot, err := NewSlot("2", firstSlotIndex)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	slot.setNetNSPath(t.TempDir() + "/ns-2")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.handleCreatedSlotAfterCancel(ctx, slot)

	if storage.released != 1 {
		t.Fatalf("Release count = %d, want 1", storage.released)
	}
	if _, ok := store.records[slot.Key]; ok {
		t.Fatalf("slot record still exists after discard")
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

func TestPoolInUseTracking(t *testing.T) {
	p := &Pool{inUse: make(map[string]*Slot)}
	slot := &Slot{Key: "2"}

	p.trackInUse(slot)
	if got := p.inUse["2"]; got != slot {
		t.Fatalf("trackInUse did not store slot")
	}
	p.untrackInUse(slot)
	if _, ok := p.inUse["2"]; ok {
		t.Fatalf("untrackInUse did not remove slot")
	}

	p.trackInUse(slot)
	drained := p.drainInUse()
	if len(drained) != 1 || drained[0] != slot {
		t.Fatalf("drainInUse() = %#v, want tracked slot", drained)
	}
	if len(p.inUse) != 0 {
		t.Fatalf("inUse len after drain = %d, want 0", len(p.inUse))
	}
}

func TestEnqueueReplacementReturnsWhenWarmPoolIsAlreadyFull(t *testing.T) {
	p := &Pool{
		newSlots: make(chan *Slot, 1),
		done:     make(chan struct{}),
	}
	p.newSlots <- &Slot{Key: "idle"}

	result := make(chan error, 1)
	go func() {
		result <- p.enqueueReplacement(context.Background(), &Slot{Key: "released"})
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "already full") {
			t.Fatalf("enqueueReplacement() error = %v, want pool already full", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("enqueueReplacement() blocked when the warm pool was already full")
	}
}

func TestReleaseDiscardsExcessSlotWhenWarmPoolIsAlreadyFull(t *testing.T) {
	store := newFakeNetworkSlotStore()
	storage := &fakeStorage{}
	p := &Pool{
		slotStorage: storage,
		slotStore:   store,
		newSlots:    make(chan *Slot, 1),
		done:        make(chan struct{}),
		inUse:       make(map[string]*Slot),
		slotHealthCheck: func(context.Context, *Slot) error {
			return nil
		},
	}
	p.newSlots <- &Slot{Key: "idle"}
	released := &Slot{Key: "released", Idx: firstSlotIndex}
	released.assignSandbox("sandbox-a")
	released.setNetNSPath(t.TempDir() + "/missing-netns")
	p.trackInUse(released)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Release(ctx, released); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if storage.released != 1 {
		t.Fatalf("storage Release count = %d, want 1", storage.released)
	}
	if storage.acquired != 0 {
		t.Fatalf("storage Acquire count = %d, want no replacement allocation", storage.acquired)
	}
	if len(p.inUse) != 0 {
		t.Fatalf("in-use slots = %#v, want empty", p.inUse)
	}
	if _, ok := store.records[released.Key]; ok {
		t.Fatalf("released excess slot record still exists")
	}
	if got := <-p.newSlots; got.Key != "idle" {
		t.Fatalf("warm slot = %q, want existing idle slot", got.Key)
	}
}

func TestRestoreInUseRejectsMissingNamespace(t *testing.T) {
	store := newFakeNetworkSlotStore()
	p := &Pool{
		cniManager: &CNIManager{config: CNIManagerConfig{IfName: defaultCNIIfName}},
		slotStore:  store,
		inUse:      make(map[string]*Slot),
	}
	slot, err := NewSlot("2", firstSlotIndex)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	slot.setNetNSPath(t.TempDir() + "/ns-2")

	err = p.RestoreInUse(slot, "sandbox-a", "10.12.0.2")
	if err == nil || !strings.Contains(err.Error(), "namespace missing") {
		t.Fatalf("RestoreInUse() error = %v, want namespace missing", err)
	}
	if slot.CNIResult() != nil {
		t.Fatalf("CNIResult() = %#v, want nil after failed restore", slot.CNIResult())
	}
	if got := p.inUse["2"]; got != nil {
		t.Fatalf("inUse[2] = %#v, want nil after failed restore", got)
	}
	rec := store.records[slot.Key]
	if rec.State != state.NetworkSlotCleaning {
		t.Fatalf("slot record state = %q, want %q", rec.State, state.NetworkSlotCleaning)
	}
	if rec.SandboxID != "sandbox-a" {
		t.Fatalf("slot record SandboxID = %q, want sandbox-a", rec.SandboxID)
	}
	if !strings.Contains(rec.LastError, "namespace missing") {
		t.Fatalf("slot record LastError = %q, want namespace missing", rec.LastError)
	}
}

func TestAdoptWarmIdleCleansCreatingAndCleaningRecords(t *testing.T) {
	netnsDir := t.TempDir()
	store := newFakeNetworkSlotStore(
		state.NetworkSlotRecord{SlotKey: "2", SlotIndex: firstSlotIndex, State: state.NetworkSlotCreating, NetNSPath: netnsDir + "/ns-2"},
		state.NetworkSlotRecord{SlotKey: "3", SlotIndex: firstSlotIndex + 1, State: state.NetworkSlotCleaning, NetNSPath: netnsDir + "/ns-3"},
	)
	storage := &fakeStorage{}
	p := &Pool{
		slotStorage: storage,
		slotStore:   store,
		newSlots:    make(chan *Slot, 2),
		inUse:       make(map[string]*Slot),
	}

	adopted, err := p.AdoptWarmIdle(context.Background())
	if err != nil {
		t.Fatalf("AdoptWarmIdle() error = %v", err)
	}
	if adopted != 0 {
		t.Fatalf("AdoptWarmIdle() adopted = %d, want 0", adopted)
	}
	if storage.released != 2 {
		t.Fatalf("Release count = %d, want 2", storage.released)
	}
	if len(store.records) != 0 {
		t.Fatalf("remaining slot records = %#v, want none", store.records)
	}
}

func TestAdoptWarmIdleCleansInvalidWarmRecord(t *testing.T) {
	netnsDir := t.TempDir()
	store := newFakeNetworkSlotStore(
		state.NetworkSlotRecord{SlotKey: "2", SlotIndex: firstSlotIndex, State: state.NetworkSlotWarmIdle, NetNSPath: netnsDir + "/ns-2"},
	)
	storage := &fakeStorage{}
	p := &Pool{
		slotStorage: storage,
		slotStore:   store,
		cniManager:  &CNIManager{config: CNIManagerConfig{IfName: defaultCNIIfName}},
		newSlots:    make(chan *Slot, 1),
		inUse:       make(map[string]*Slot),
	}

	adopted, err := p.AdoptWarmIdle(context.Background())
	if err != nil {
		t.Fatalf("AdoptWarmIdle() error = %v", err)
	}
	if adopted != 0 {
		t.Fatalf("AdoptWarmIdle() adopted = %d, want 0", adopted)
	}
	if storage.released != 1 {
		t.Fatalf("Release count = %d, want 1", storage.released)
	}
	if len(store.records) != 0 {
		t.Fatalf("remaining slot records = %#v, want none", store.records)
	}
}

func TestGetRequeuesSlotWhenAssignmentRecordFails(t *testing.T) {
	slot, err := NewSlot("2", firstSlotIndex)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
	}
	storeErr := errors.New("store unavailable")
	p := &Pool{
		slotStore: storeErrNetworkSlotStore(storeErr),
		newSlots:  make(chan *Slot, 1),
		inUse:     make(map[string]*Slot),
	}
	p.newSlots <- slot

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
	if len(p.inUse) != 0 {
		t.Fatalf("inUse len = %d, want 0", len(p.inUse))
	}
	select {
	case requeued := <-p.newSlots:
		if requeued != slot {
			t.Fatalf("requeued slot = %#v, want original slot", requeued)
		}
	default:
		t.Fatalf("slot was not requeued after assignment record failure")
	}
}

func TestCleanupAssignedWithoutReadySandbox(t *testing.T) {
	netnsDir := t.TempDir()
	store := newFakeNetworkSlotStore(
		state.NetworkSlotRecord{SlotKey: "2", SlotIndex: firstSlotIndex, State: state.NetworkSlotAssigned, SandboxID: "sandbox-gone", NetNSPath: netnsDir + "/ns-2"},
		state.NetworkSlotRecord{SlotKey: "3", SlotIndex: firstSlotIndex + 1, State: state.NetworkSlotAssigned, SandboxID: "sandbox-ready", NetNSPath: netnsDir + "/ns-3"},
	)
	storage := &fakeStorage{}
	p := &Pool{
		slotStorage: storage,
		slotStore:   store,
		inUse:       make(map[string]*Slot),
	}

	err := p.CleanupAssignedWithoutReadySandbox(map[string]struct{}{"sandbox-ready": {}})
	if err != nil {
		t.Fatalf("CleanupAssignedWithoutReadySandbox() error = %v", err)
	}
	if storage.released != 1 {
		t.Fatalf("Release count = %d, want 1", storage.released)
	}
	if _, ok := store.records["2"]; ok {
		t.Fatalf("stale assigned slot record still exists")
	}
	if rec, ok := store.records["3"]; !ok || rec.SandboxID != "sandbox-ready" {
		t.Fatalf("ready assigned slot record = %#v, ok=%v; want preserved", rec, ok)
	}
}

func storeErrNetworkSlotStore(err error) *fakeNetworkSlotStore {
	return &fakeNetworkSlotStore{records: make(map[string]state.NetworkSlotRecord), upsertErr: err}
}

func TestDiscardDuringShutdownDoesNotReplenish(t *testing.T) {
	storage := &fakeStorage{}
	p := &Pool{
		slotStorage: storage,
		newSlots:    make(chan *Slot, 1),
		done:        make(chan struct{}),
		inUse:       make(map[string]*Slot),
	}
	slot := &Slot{Key: "2", Idx: 2}
	slot.setNetNSPath(t.TempDir() + "/missing-netns")
	p.trackInUse(slot)

	close(p.done)
	if err := p.Discard(context.Background(), slot); err != nil {
		t.Fatalf("Discard() error = %v", err)
	}
	if storage.released != 1 {
		t.Fatalf("Release count = %d, want 1", storage.released)
	}
	if storage.acquired != 0 {
		t.Fatalf("Acquire count = %d, want 0", storage.acquired)
	}
	if len(p.inUse) != 0 {
		t.Fatalf("inUse len = %d, want 0", len(p.inUse))
	}
}
