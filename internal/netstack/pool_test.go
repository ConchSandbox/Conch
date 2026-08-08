package netstack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func integrationTestPool(t *testing.T) *Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("network slot integration test is disabled in short mode")
	}
	if os.Geteuid() != 0 {
		t.Skip("network slot integration test requires root privileges")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skipf("network slot integration test requires /dev/net/tun: %v", err)
	}
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skipf("network slot integration test requires iptables: %v", err)
	}

	pluginDir := integrationCNIPluginDir()
	if pluginDir == "" {
		t.Skip("network slot integration test requires loopback, bridge, and host-local CNI plugins")
	}
	confDir := t.TempDir()
	octet := os.Getpid()%200 + 20
	conf := fmt.Sprintf(`{
  "cniVersion": "1.0.0",
  "name": "conch-integration-%d",
  "type": "bridge",
  "bridge": "ct-%x",
  "isGateway": true,
  "ipMasq": true,
  "ipam": {
    "type": "host-local",
    "subnet": "10.254.%d.0/24",
    "routes": [{"dst": "0.0.0.0/0"}]
  }
}
`, os.Getpid(), os.Getpid(), octet)
	if err := os.WriteFile(filepath.Join(confDir, "10-conch-integration.conf"), []byte(conf), 0o600); err != nil {
		t.Fatalf("write integration CNI config: %v", err)
	}

	p, err := NewPool(1, "", 0, CNIManagerConfig{
		PluginBinDirs: []string{pluginDir},
		PluginConfDir: confDir,
		IfName:        defaultCNIIfName,
	})
	if err != nil {
		t.Fatalf("initialize integration network pool: %v", err)
	}
	return p
}

func integrationCNIPluginDir() string {
	candidates := []string{
		os.Getenv("CONCH_TEST_CNI_BIN_DIR"),
		"/opt/cni/bin",
		"/usr/lib/cni",
		"/usr/libexec/cni",
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		available := true
		for _, plugin := range []string{"loopback", "bridge", "host-local"} {
			info, err := os.Stat(filepath.Join(dir, plugin))
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				available = false
				break
			}
		}
		if available {
			return dir
		}
	}
	return ""
}

func integrationTestSlot(t *testing.T) (*Pool, *Slot) {
	t.Helper()
	p := integrationTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	slot, err := p.createNetworkSlot(ctx)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrExist) || strings.Contains(strings.ToLower(err.Error()), "operation not permitted") {
			t.Skipf("network slot integration environment is unavailable: %v", err)
		}
		t.Fatalf("create integration network slot: %v", err)
	}
	t.Cleanup(func() {
		if slot.CNIIP() == "" {
			if _, statErr := os.Stat(slot.NetNSPath()); os.IsNotExist(statErr) {
				return
			}
		}
		if cleanupErr := p.destroyNetworkSlot(context.Background(), slot); cleanupErr != nil {
			t.Errorf("clean up integration network slot: %v", cleanupErr)
		}
	})
	return p, slot
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

func TestNetworkSlotIntegrationDestroyKeepsIDReservedWithoutSignalAfterCNIDelFailure(t *testing.T) {
	p, slot := integrationTestSlot(t)
	allocator := p.slotIDs
	removeErr := errors.New("cni del failed")
	removeCalls := 0
	realPlugin := p.cniManager.plugin
	p.cniManager.plugin = &fakeCNIPlugin{
		remove: func(context.Context, string, string, ...cni.NamespaceOpts) error {
			removeCalls++
			return removeErr
		},
	}
	defer func() { p.cniManager.plugin = realPlugin }()

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

func TestNetworkSlotIntegrationDestroyReleasesID(t *testing.T) {
	p, slot := integrationTestSlot(t)
	allocator := p.slotIDs

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

func TestNetworkSlotIntegrationPoolCloseRejectsGetAndCleansBufferedSlots(t *testing.T) {
	p, slot := integrationTestSlot(t)
	allocator := p.slotIDs
	if err := p.warmSlots.Push(slot); err != nil {
		t.Fatalf("Push(): %v", err)
	}

	p.Close()
	p.Close()

	if _, err := p.Get(context.Background(), "sandbox-a"); !errors.Is(err, errWarmPoolClosed) {
		t.Fatalf("Get() after Close() error = %v, want errWarmPoolClosed", err)
	}
	size, _ := p.warmSlots.Usage()
	if size != 0 || !p.warmSlots.IsClosed() {
		t.Fatalf("warm queue after Close() = (size=%d, closed=%v), want empty and closed", size, p.warmSlots.IsClosed())
	}
	reused, err := allocator.Acquire()
	if err != nil || reused != slot.ID() {
		t.Fatalf("Acquire() after Close() = (%d, %v), want (%d, nil)", reused, err, slot.ID())
	}
}

func TestNetworkSlotIntegrationPoolCloseWaitsForPopulationBeforeCleaningBufferedSlots(t *testing.T) {
	p, slot := integrationTestSlot(t)
	populateCanceled := make(chan struct{})
	populateDone := make(chan struct{})
	removeCalled := make(chan struct{}, 1)
	p.populateCancel = func() { close(populateCanceled) }
	p.populateDone = populateDone
	realPlugin := p.cniManager.plugin
	p.cniManager.plugin = &fakeCNIPlugin{
		setup: realPlugin.Setup,
		remove: func(ctx context.Context, id, path string, opts ...cni.NamespaceOpts) error {
			removeCalled <- struct{}{}
			return realPlugin.Remove(ctx, id, path, opts...)
		},
	}
	defer func() { p.cniManager.plugin = realPlugin }()
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

func TestNetworkSlotIntegrationDiscardWakesPopulateRetryAfterCapacityRelease(t *testing.T) {
	p, slot := integrationTestSlot(t)
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
