package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	types100 "github.com/containernetworking/cni/pkg/types/100"
	slotstate "github.com/openeuler/Conch/internal/netstack/slot"
	"github.com/vishvananda/netlink"
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
			if _, err := NewPool(PoolConfig{WarmPoolSize: tt.warm}); err == nil || !strings.Contains(err.Error(), "network.warm_pool_size") {
				t.Fatalf("NewPool() error = %v, want invalid warm pool size error", err)
			}
		})
	}
}

type fakeCNIBackend struct {
	setup       func(context.Context, string, string) (*types100.Result, error)
	remove      func(context.Context, string, string) error
	attachments func() ([]cniAttachment, error)
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

func (f *fakeCNIBackend) Setup(ctx context.Context, id, path string) (*types100.Result, error) {
	if f.setup == nil {
		return nil, nil
	}
	return f.setup(ctx, id, path)
}

func (f *fakeCNIBackend) Remove(ctx context.Context, id, path string) error {
	if f.remove == nil {
		return nil
	}
	return f.remove(ctx, id, path)
}

func (f *fakeCNIBackend) CachedAttachments() ([]cniAttachment, error) {
	if f.attachments == nil {
		return nil, nil
	}
	return f.attachments()
}

func allocatedTestSlot(t *testing.T) (*slotstate.Allocator, *Slot) {
	t.Helper()
	allocator := slotstate.NewAllocator(firstSlotID, maxSlots)
	id, err := allocator.Acquire()
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	cfg := newSlotConfig()
	slot, err := newSlot(id, cfg)
	if err != nil {
		t.Fatalf("newSlot(): %v", err)
	}
	return allocator, slot
}

func integrationTestPool(t *testing.T) *Pool {
	return integrationTestPoolWithIPMasq(t, true)
}

func integrationTestPoolWithIPMasq(t *testing.T, ipMasq bool) *Pool {
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
		t.Skip("network slot integration test requires bridge and host-local CNI plugins")
	}
	confDir := t.TempDir()
	octet := os.Getpid()%200 + 20
	conf := fmt.Sprintf(`{
  "cniVersion": "1.0.0",
  "name": "conch-integration-%d",
  "type": "bridge",
  "bridge": "ct-%x",
  "isGateway": true,
  "ipMasq": %t,
  "ipam": {
    "type": "host-local",
	"dataDir": %q,
    "subnet": "10.254.%d.0/24",
    "routes": [{"dst": "0.0.0.0/0"}]
  }
}
`, os.Getpid(), os.Getpid(), ipMasq, filepath.Join(cniCacheDir, "networks"), octet)
	if err := os.WriteFile(filepath.Join(confDir, "10-conch-integration.conf"), []byte(conf), 0o600); err != nil {
		t.Fatalf("write integration CNI config: %v", err)
	}

	p, err := NewPool(PoolConfig{
		WarmPoolSize: 1,
		CNI: CNIManagerConfig{
			PluginBinDirs: []string{pluginDir},
			PluginConfDir: confDir,
		},
	})
	if err != nil {
		t.Fatalf("initialize integration network pool: %v", err)
	}
	t.Cleanup(func() {
		if err := removeHostForwardingRules(p.cniManager.bridgeName, p.hostInterface); err != nil {
			t.Errorf("remove integration host forwarding rules: %v", err)
		}
		if link, err := netlink.LinkByName(p.cniManager.bridgeName); err == nil {
			if err := netlink.LinkDel(link); err != nil {
				t.Errorf("remove integration CNI bridge: %v", err)
			}
		}
		removeIntegrationIPAMMetadata(t, p)
	})
	return p
}

func removeIntegrationIPAMMetadata(t *testing.T, p *Pool) {
	t.Helper()
	dir := filepath.Join(cniCacheDir, "networks", p.cniManager.bridgeNetworkName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Errorf("read integration IPAM metadata: %v", err)
		}
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || (entry.Name() != "lock" && !strings.HasPrefix(entry.Name(), "last_reserved_ip.")) {
			t.Errorf("refusing to remove unexpected integration IPAM entry %s", filepath.Join(dir, entry.Name()))
			return
		}
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove integration IPAM metadata %s: %v", entry.Name(), err)
			return
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		t.Errorf("remove integration IPAM directory: %v", err)
	}
}

func integrationCNIPluginDir() string {
	candidates := []string{
		os.Getenv("CONCH_TEST_CNI_BIN_DIR"),
		defaultCNIPluginBinDir,
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		available := true
		for _, plugin := range []string{"bridge", "host-local"} {
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
	return p, integrationTestSlotInPool(t, p)
}

func integrationTestSlotInPool(t *testing.T, p *Pool) *Slot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	slot, err := p.createNetworkSlot(ctx)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrExist) || strings.Contains(strings.ToLower(err.Error()), "operation not permitted") {
			t.Skipf("network slot integration environment is unavailable: %v", err)
		}
		t.Fatalf("create integration network slot: %v", err)
	}
	if err := runInNetNSPath(ctx, slot.NetNSPath(), func() error {
		lo, err := netlink.LinkByName(loopbackInterface)
		if err != nil {
			return err
		}
		if lo.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("loopback interface is down")
		}
		return nil
	}); err != nil {
		t.Fatalf("verify loopback interface: %v", err)
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
	return slot
}

func TestSetupSlotNetworkLeavesRollbackToPool(t *testing.T) {
	_, slot := allocatedTestSlot(t)
	setupErr := errors.New("cni add failed")
	removeCalls := 0
	backend := &fakeCNIBackend{
		setup: func(context.Context, string, string) (*types100.Result, error) {
			return nil, setupErr
		},
		remove: func(context.Context, string, string) error {
			removeCalls++
			return nil
		},
	}
	p := &Pool{cniManager: &CNIManager{
		backend: backend,
		ifName:  cniOuterInterfaceName,
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

func TestReleaseDiscardsSlotWithMissingNamespace(t *testing.T) {
	allocator, slot := allocatedTestSlot(t)
	slot.recordCNIResult(CNIResult{IP: "10.12.0.10"})
	slot.assignSandbox("sandbox-1")
	removeCalls := 0
	p := &Pool{
		cniManager: &CNIManager{backend: &fakeCNIBackend{
			remove: func(_ context.Context, _ string, path string) error {
				removeCalls++
				if path != "" {
					t.Fatalf("CNI Remove path = %q, want empty for missing namespace", path)
				}
				return nil
			},
		}},
		slotIDs:      allocator,
		refillNeeded: make(chan struct{}, 1),
	}

	if err := p.Release(context.Background(), slot); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if removeCalls != 1 {
		t.Fatalf("CNI Remove calls = %d, want 1", removeCalls)
	}
	if reused, err := allocator.Acquire(); err != nil || reused != slot.ID() {
		t.Fatalf("Acquire() after release = (%d, %v), want (%d, nil)", reused, err, slot.ID())
	}
}

func TestReleaseMissingNamespaceKeepsSlotIDWhenCNITeardownFails(t *testing.T) {
	allocator, slot := allocatedTestSlot(t)
	slot.recordCNIResult(CNIResult{IP: "10.12.0.10"})
	wantErr := errors.New("cni remove failed")
	p := &Pool{
		cniManager: &CNIManager{backend: &fakeCNIBackend{
			remove: func(context.Context, string, string) error {
				return wantErr
			},
		}},
		slotIDs: allocator,
	}

	if err := p.Release(context.Background(), slot); !errors.Is(err, wantErr) {
		t.Fatalf("Release() error = %v, want %v", err, wantErr)
	}
	if reused, err := allocator.Acquire(); err != nil || reused == slot.ID() {
		t.Fatalf("Acquire() after failed cleanup = (%d, %v), slot ID %d must remain reserved", reused, err, slot.ID())
	}
}

func TestTeardownDerivesCNIIdentityFromSlot(t *testing.T) {
	_, slot := allocatedTestSlot(t)
	removeCalls := 0
	removeErr := errors.New("cni del failed")
	p := &Pool{cniManager: &CNIManager{backend: &fakeCNIBackend{
		remove: func(_ context.Context, id, path string) error {
			removeCalls++
			if id != slot.cniContainerID() || path != "" {
				t.Fatalf("Remove(%q, %q), want (%q, empty)", id, path, slot.cniContainerID())
			}
			return removeErr
		},
	}}}
	if err := p.teardownSlotNetwork(context.Background(), slot); !errors.Is(err, removeErr) {
		t.Fatalf("teardownSlotNetwork() error = %v, want %v", err, removeErr)
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
	realBackend := p.cniManager.backend
	p.cniManager.backend = &fakeCNIBackend{
		remove: func(context.Context, string, string) error {
			removeCalls++
			return removeErr
		},
	}
	defer func() { p.cniManager.backend = realBackend }()

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

func TestNetworkSlotIntegrationStartupReconcilesCacheWithoutNetNS(t *testing.T) {
	p := integrationTestPoolWithIPMasq(t, false)
	slot := integrationTestSlotInPool(t, p)
	cacheFile := filepath.Join(cniCacheDir, "results", p.cniManager.bridgeNetworkName+"-"+slot.cniContainerID()+"-"+p.cniManager.ifName)
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("CNI ADD did not create result cache %s: %v", cacheFile, err)
	}
	ipamDir := filepath.Join(cniCacheDir, "networks", p.cniManager.bridgeNetworkName)
	if allocations := integrationIPAMAllocations(t, ipamDir); len(allocations) == 0 {
		t.Fatalf("CNI ADD created no host-local allocation in %s", ipamDir)
	}

	// Simulate the reboot boundary: /run loses the mounted netns while libcni
	// result cache and host-local IPAM state remain persistent.
	if err := deleteNetworkNamespace(slot); err != nil {
		t.Fatalf("delete integration netns before startup reconciliation: %v", err)
	}
	if err := p.CleanupStaleResources(context.Background()); err != nil {
		t.Fatalf("CleanupStaleResources(): %v", err)
	}

	if _, err := os.Stat(cacheFile); !os.IsNotExist(err) {
		t.Errorf("stale CNI cache %s remains, stat error = %v", cacheFile, err)
	}
	if allocations := integrationIPAMAllocations(t, ipamDir); len(allocations) != 0 {
		t.Errorf("stale host-local allocations remain: %v", allocations)
	}
	if _, err := netlink.LinkByName(p.cniManager.bridgeName); err == nil {
		t.Errorf("stale CNI bridge %s remains", p.cniManager.bridgeName)
	}
}

func integrationIPAMAllocations(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read host-local IPAM directory %s: %v", dir, err)
	}
	var allocations []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "lock" || strings.HasPrefix(entry.Name(), "last_reserved_ip.") {
			continue
		}
		allocations = append(allocations, entry.Name())
	}
	return allocations
}

func TestNetworkSlotIntegrationPoolCloseRejectsGetAndCleansBufferedSlots(t *testing.T) {
	p, slot := integrationTestSlot(t)
	allocator := p.slotIDs
	if err := p.warmSlots.Push(slot); err != nil {
		t.Fatalf("Push(): %v", err)
	}

	p.Close()
	p.Close()

	if _, err := p.Get(context.Background(), "sandbox-a", nil); !errors.Is(err, errWarmPoolClosed) {
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
	realBackend := p.cniManager.backend
	p.cniManager.backend = &fakeCNIBackend{
		setup: realBackend.Setup,
		remove: func(ctx context.Context, id, path string) error {
			removeCalled <- struct{}{}
			return realBackend.Remove(ctx, id, path)
		},
		attachments: realBackend.CachedAttachments,
	}
	defer func() { p.cniManager.backend = realBackend }()
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

	got, err := p.Get(context.Background(), "sandbox-a", nil)
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
