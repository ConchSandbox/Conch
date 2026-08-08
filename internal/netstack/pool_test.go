package netstack

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cni "github.com/containerd/go-cni"
	slotstate "github.com/openeuler/Conch/internal/netstack/slot"
)

func TestNewPoolRejectsInvalidWarmPoolSize(t *testing.T) {
	tests := []struct {
		name string
		warm int
	}{
		{name: "exceeds maximum", warm: maxSlots + 1},
		{name: "negative", warm: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewPool(tt.warm, "", 0, CNIManagerConfig{}); err == nil || !strings.Contains(err.Error(), "network.warm_pool_size") {
				t.Fatalf("NewPool() error = %v, want invalid warm pool size error", err)
			}
		})
	}
}

type fakeCNIPlugin struct {
	setup  func(context.Context, string, string, ...cni.NamespaceOpts) (*cni.Result, error)
	remove func(context.Context, string, string, ...cni.NamespaceOpts) error
}

func TestCreateNetworkSlotWithRetrySucceedsOnSecondAttempt(t *testing.T) {
	wantSlot := &Slot{id: firstSlotID}
	attempts := 0
	slot, err := createNetworkSlotWithRetry(context.Background(), func(context.Context) (*Slot, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("stale CNI allocation")
		}
		return wantSlot, nil
	})
	if err != nil {
		t.Fatalf("createNetworkSlotWithRetry() error = %v", err)
	}
	if slot != wantSlot || attempts != 2 {
		t.Fatalf("result = (%p, attempts=%d), want (%p, attempts=2)", slot, attempts, wantSlot)
	}
}

func TestCreateNetworkSlotWithRetryReturnsSecondError(t *testing.T) {
	wantErr := errors.New("CNI still unavailable")
	attempts := 0
	slot, err := createNetworkSlotWithRetry(context.Background(), func(context.Context) (*Slot, error) {
		attempts++
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("createNetworkSlotWithRetry() error = %v, want %v", err, wantErr)
	}
	if slot != nil || attempts != 2 {
		t.Fatalf("result = (%p, attempts=%d), want (nil, attempts=2)", slot, attempts)
	}
}

func (f *fakeCNIPlugin) GetConfig() *cni.ConfigResult { return nil }

func (f *fakeCNIPlugin) Setup(ctx context.Context, id, path string, opts ...cni.NamespaceOpts) (*cni.Result, error) {
	if f.setup == nil {
		return nil, nil
	}
	return f.setup(ctx, id, path, opts...)
}

func (f *fakeCNIPlugin) Remove(ctx context.Context, id, path string, opts ...cni.NamespaceOpts) error {
	if f.remove == nil {
		return nil
	}
	return f.remove(ctx, id, path, opts...)
}

func allocatedTestSlot(t *testing.T) (*slotstate.Allocator, *Slot) {
	t.Helper()
	allocator := slotstate.NewAllocator(firstSlotID, maxSlots)
	id, err := allocator.Acquire()
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	cfg, err := newSlotConfig("", 0)
	if err != nil {
		t.Fatalf("newSlotConfig(): %v", err)
	}
	slot, err := newSlot(id, cfg)
	if err != nil {
		t.Fatalf("newSlot(): %v", err)
	}
	return allocator, slot
}

func TestSetupSlotNetworkLeavesRollbackToPool(t *testing.T) {
	_, slot := allocatedTestSlot(t)
	setupErr := errors.New("cni add failed")
	removeCalls := 0
	plugin := &fakeCNIPlugin{
		setup: func(context.Context, string, string, ...cni.NamespaceOpts) (*cni.Result, error) {
			return nil, setupErr
		},
		remove: func(context.Context, string, string, ...cni.NamespaceOpts) error {
			removeCalls++
			return nil
		},
	}
	p := &Pool{cniManager: &CNIManager{
		plugin: plugin,
		config: CNIManagerConfig{IfName: defaultCNIIfName},
	}}

	err := p.provisionSlotNetwork(context.Background(), slot)
	if !errors.Is(err, setupErr) {
		t.Fatalf("provisionSlotNetwork() error = %v, want %v", err, setupErr)
	}
	if removeCalls != 0 {
		t.Fatalf("CNI Remove calls during setup = %d, want 0; Pool cleanup owns DEL", removeCalls)
	}
	if slot.CNIIP() != "" {
		t.Fatalf("slot CNI IP after failed ADD = %q, want empty", slot.CNIIP())
	}
}

func TestTeardownDerivesCNIIdentityFromSlot(t *testing.T) {
	_, slot := allocatedTestSlot(t)
	removeCalls := 0
	p := &Pool{cniManager: &CNIManager{plugin: &fakeCNIPlugin{
		remove: func(_ context.Context, id, path string, _ ...cni.NamespaceOpts) error {
			removeCalls++
			if id != slot.cniContainerID() || path != "" {
				t.Fatalf("Remove(%q, %q), want (%q, empty)", id, path, slot.cniContainerID())
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

func TestDestroyNetworkSlotKeepsIDReservedWithoutSignalAfterCNIDelFailure(t *testing.T) {
	allocator, slot := allocatedTestSlot(t)
	removeErr := errors.New("cni del failed")
	removeCalls := 0
	p := &Pool{
		refillNeeded: make(chan struct{}, 1),
		slotIDs:      allocator,
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{
			remove: func(context.Context, string, string, ...cni.NamespaceOpts) error {
				removeCalls++
				return removeErr
			},
		}},
	}

	err := p.destroyNetworkSlot(context.Background(), slot)
	if !errors.Is(err, removeErr) {
		t.Fatalf("destroyNetworkSlot() error = %v, want %v", err, removeErr)
	}
	if removeCalls != 1 {
		t.Fatalf("CNI Remove calls = %d, want 1", removeCalls)
	}
	reused, acquireErr := allocator.Acquire()
	if acquireErr != nil {
		t.Fatalf("Acquire() after failed cleanup: %v", acquireErr)
	}
	if reused == slot.ID() {
		t.Fatalf("slot ID %d was released after failed CNI DEL", slot.ID())
	}
	select {
	case <-p.refillNeeded:
		t.Fatal("destroyNetworkSlot() signaled refill even though the slot ID remained reserved")
	default:
	}
}

func TestDestroyNetworkSlotReleasesID(t *testing.T) {
	allocator, slot := allocatedTestSlot(t)
	p := &Pool{
		slotIDs:    allocator,
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{}},
	}

	if err := p.destroyNetworkSlot(context.Background(), slot); err != nil {
		t.Fatalf("destroyNetworkSlot(): %v", err)
	}

	reused, err := allocator.Acquire()
	if err != nil || reused != slot.ID() {
		t.Fatalf("Acquire() after discard = (%d, %v), want (%d, nil)", reused, err, slot.ID())
	}
}

func TestCNIBusyErrorDetection(t *testing.T) {
	if !isCNIBusyTeardownError(errors.New("CHAIN_DEL failed: Device or resource busy")) {
		t.Fatal("isCNIBusyTeardownError() = false, want true")
	}
	if isCNIBusyTeardownError(errors.New("some other cni error")) {
		t.Fatal("isCNIBusyTeardownError(non-busy) = true, want false")
	}
}

func TestPoolCloseRejectsGetAndCleansBufferedSlots(t *testing.T) {
	allocator, slot := allocatedTestSlot(t)
	removeCalls := 0
	p := &Pool{
		warmSlots:    slotstate.NewQueue[*Slot](1),
		refillNeeded: make(chan struct{}, 1),
		slotIDs:      allocator,
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{
			remove: func(context.Context, string, string, ...cni.NamespaceOpts) error {
				removeCalls++
				return nil
			},
		}},
	}
	if err := p.warmSlots.Push(slot); err != nil {
		t.Fatalf("Push(): %v", err)
	}
	p.Start(context.Background())

	p.Close()
	p.Close()

	if _, err := p.Get(context.Background(), "sandbox-a"); !errors.Is(err, errWarmPoolClosed) {
		t.Fatalf("Get() after Close() error = %v, want errWarmPoolClosed", err)
	}
	size, _ := p.warmSlots.Usage()
	if size != 0 || !p.warmSlots.IsClosed() {
		t.Fatalf("warm queue after Close() = (size=%d, closed=%v), want empty and closed", size, p.warmSlots.IsClosed())
	}
	if removeCalls != 1 {
		t.Fatalf("CNI Remove calls = %d, want 1", removeCalls)
	}
	reused, err := allocator.Acquire()
	if err != nil || reused != slot.ID() {
		t.Fatalf("Acquire() after Close() = (%d, %v), want (%d, nil)", reused, err, slot.ID())
	}
}

func TestPoolCloseWaitsForPopulationBeforeCleaningBufferedSlots(t *testing.T) {
	allocator, slot := allocatedTestSlot(t)
	populateCanceled := make(chan struct{})
	populateDone := make(chan struct{})
	removeCalled := make(chan struct{}, 1)
	p := &Pool{
		warmSlots:      slotstate.NewQueue[*Slot](1),
		populateCancel: func() { close(populateCanceled) },
		populateDone:   populateDone,
		slotIDs:        allocator,
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{
			remove: func(context.Context, string, string, ...cni.NamespaceOpts) error {
				removeCalled <- struct{}{}
				return nil
			},
		}},
	}
	if err := p.warmSlots.Push(slot); err != nil {
		t.Fatalf("Push(): %v", err)
	}

	closeReturned := make(chan struct{})
	go func() {
		p.Close()
		close(closeReturned)
	}()
	<-populateCanceled
	select {
	case <-removeCalled:
		t.Fatal("buffered slot was cleaned before population exited")
	default:
	}

	close(populateDone)
	<-closeReturned
	select {
	case <-removeCalled:
	default:
		t.Fatal("buffered slot was not cleaned after population exited")
	}
}

func TestDiscardWakesPopulateRetryAfterCapacityRelease(t *testing.T) {
	allocator, slot := allocatedTestSlot(t)
	p := &Pool{
		refillNeeded: make(chan struct{}, 1),
		slotIDs:      allocator,
		cniManager:   &CNIManager{plugin: &fakeCNIPlugin{}},
	}
	retryDone := make(chan bool, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		retryDone <- p.waitForPopulateRetry(ctx, time.Hour)
	}()

	if err := p.Discard(context.Background(), slot); err != nil {
		t.Fatalf("Discard(): %v", err)
	}
	select {
	case retry := <-retryDone:
		if !retry {
			t.Fatal("waitForPopulateRetry() stopped instead of retrying")
		}
	case <-time.After(time.Second):
		t.Fatal("successful Discard() did not wake populate retry")
	}
}

func TestGetAssignsWarmSlot(t *testing.T) {
	_, slot := allocatedTestSlot(t)
	p := &Pool{
		warmSlots:    slotstate.NewQueue[*Slot](1),
		refillNeeded: make(chan struct{}, 1),
	}
	if err := p.warmSlots.Push(slot); err != nil {
		t.Fatalf("Push(): %v", err)
	}

	got, err := p.Get(context.Background(), "sandbox-a")
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got != slot || got.sandboxID != "sandbox-a" {
		t.Fatalf("Get() = %#v, sandbox ID %q", got, got.sandboxID)
	}
	select {
	case <-p.refillNeeded:
	default:
		t.Fatal("Get() did not signal refill")
	}
}
