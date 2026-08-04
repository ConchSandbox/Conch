package netstack

import (
	"context"
	"errors"
	"strings"
	"testing"

	cni "github.com/containerd/go-cni"
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

type fakeCNIPlugin struct {
	setup  func(context.Context, string, string, ...cni.NamespaceOpts) (*cni.Result, error)
	remove func(context.Context, string, string, ...cni.NamespaceOpts) error
	check  func(context.Context, string, string, ...cni.NamespaceOpts) error
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

func (f *fakeCNIPlugin) Check(ctx context.Context, id, path string, opts ...cni.NamespaceOpts) error {
	if f.check == nil {
		return nil
	}
	return f.check(ctx, id, path, opts...)
}

func (*fakeCNIPlugin) Load(...cni.Opt) error { return nil }

func (*fakeCNIPlugin) Status() error { return nil }

func (*fakeCNIPlugin) GetConfig() *cni.ConfigResult { return nil }

func allocatedTestSlot(t *testing.T) (*slotstate.Allocator, *Slot) {
	t.Helper()
	allocator := slotstate.NewAllocator(firstSlotID, maxSlots)
	id, err := allocator.Acquire()
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	slot, err := NewSlot(id)
	if err != nil {
		t.Fatalf("NewSlot(): %v", err)
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

	err := p.setupSlotNetwork(context.Background(), slot)
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

func TestTeardownDerivesCNIIdentityFromSlot(t *testing.T) {
	_, slot := allocatedTestSlot(t)
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

func TestCleanupSlotAllocationKeepsIDReservedAfterCNIDelFailure(t *testing.T) {
	allocator, slot := allocatedTestSlot(t)
	removeErr := errors.New("cni del failed")
	removeCalls := 0
	p := &Pool{
		slotIDs: allocator,
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{
			remove: func(context.Context, string, string, ...cni.NamespaceOpts) error {
				removeCalls++
				return removeErr
			},
		}},
	}

	err := p.cleanupSlotAllocation(slot)
	if !errors.Is(err, removeErr) {
		t.Fatalf("cleanupSlotAllocation() error = %v, want %v", err, removeErr)
	}
	if removeCalls != 1 {
		t.Fatalf("CNI Remove calls = %d, want 1", removeCalls)
	}
	reused, acquireErr := allocator.Acquire()
	if acquireErr != nil {
		t.Fatalf("Acquire() after failed cleanup: %v", acquireErr)
	}
	if reused == slot.ID {
		t.Fatalf("slot ID %d was released after failed CNI DEL", slot.ID)
	}
}

func TestHandleCreatedSlotAfterCancelDiscardsSlot(t *testing.T) {
	allocator, slot := allocatedTestSlot(t)
	p := &Pool{
		slotIDs:    allocator,
		cniManager: &CNIManager{plugin: &fakeCNIPlugin{}},
	}

	p.handleCreatedSlotAfterCancel(slot)

	reused, err := allocator.Acquire()
	if err != nil || reused != slot.ID {
		t.Fatalf("Acquire() after discard = (%d, %v), want (%d, nil)", reused, err, slot.ID)
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
		warmSlots:          slotstate.NewQueue[*Slot](1),
		warmSlotNeeded:     make(chan struct{}, 1),
		dynamicReservation: true,
		prefillReady:       make(chan struct{}),
		slotIDs:            allocator,
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
	if err != nil || reused != slot.ID {
		t.Fatalf("Acquire() after Close() = (%d, %v), want (%d, nil)", reused, err, slot.ID)
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

func TestReplenishDroppedSlotSkipsClosedPool(t *testing.T) {
	p := &Pool{warmSlots: slotstate.NewQueue[*Slot](1)}
	p.warmSlots.Close()
	if err := p.replenishDroppedSlot(context.Background()); err != nil {
		t.Fatalf("replenishDroppedSlot() after close: %v", err)
	}
}

func TestGetAssignsWarmSlot(t *testing.T) {
	_, slot := allocatedTestSlot(t)
	p := &Pool{
		warmSlots:      slotstate.NewQueue[*Slot](1),
		warmSlotNeeded: make(chan struct{}, 1),
		prefillReady:   make(chan struct{}),
	}
	if err := p.warmSlots.Push(slot); err != nil {
		t.Fatalf("Push(): %v", err)
	}

	got, err := p.Get(context.Background(), "sandbox-a")
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got != slot || got.SandboxID() != "sandbox-a" {
		t.Fatalf("Get() = %#v, sandbox ID %q", got, got.SandboxID())
	}
}
