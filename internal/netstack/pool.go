/*
Copyright the e2b-dev Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

[MODIFIED] - Changes made on 2025-12-24 by Team conch: Add bridge interface
[MODIFIED] - Changes made on 2026-05-13 by Team conch: Move outer slot networking to CNI while keeping Conch-owned pool, netns, guest tap, and NAT lifecycle.
*/
package netstack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	slotstate "github.com/openeuler/Conch/internal/netstack/slot"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	DefaultWarmPoolSize   = 250
	prefillWorkers        = 16
	prefillCreateAttempts = 2
	populateRetryMinDelay = time.Second
	populateRetryMaxDelay = 30 * time.Second
	// cleanupWorkers caps concurrent teardown of buffered network slots
	// during shutdown. CNI/netns removal is I/O-bound, so a fixed cap
	// independent of the host CPU count keeps the host network stack from
	// being overwhelmed on pools with many slots.
	cleanupWorkers = 16
)

var (
	errWarmPoolClosed = slotstate.ErrClosed
	errWarmPoolEmpty  = slotstate.ErrEmpty
)

type Pool struct {
	warmSlots      *slotstate.Queue[*Slot]
	refillNeeded   chan struct{}
	populateCancel context.CancelFunc
	populateDone   <-chan struct{}
	cniManager     *CNIManager
	hostInterface  string
	slotConfig     slotConfig
	slotIDs        *slotstate.Allocator
}

type PoolConfig struct {
	WarmPoolSize int
	CNI          CNIManagerConfig
}

func NewPool(cfg PoolConfig) (*Pool, error) {
	warmPoolSize := cfg.WarmPoolSize
	if warmPoolSize == 0 {
		warmPoolSize = DefaultWarmPoolSize
	}
	if warmPoolSize < 1 || warmPoolSize > maxSlots {
		return nil, fmt.Errorf("invalid network.warm_pool_size=%d, must be within [1, %d]", warmPoolSize, maxSlots)
	}

	slotConfig := newSlotConfig()
	if err := os.MkdirAll(networkNamespaceDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare network namespace directory: create Conch network namespace directory: %w", err)
	}
	if err := os.Chmod(networkNamespaceDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare network namespace directory: secure Conch network namespace directory: %w", err)
	}
	cniManager, err := NewCNIManager(cfg.CNI)
	if err != nil {
		return nil, err
	}
	gateway, err := defaultGatewayInterface()
	if err != nil {
		return nil, err
	}
	if err := ensureHostForwardingRules(cniManager.bridgeName, gateway); err != nil {
		return nil, fmt.Errorf("configure host forwarding for CNI bridge %s: %w", cniManager.bridgeName, err)
	}

	p := &Pool{
		warmSlots:     slotstate.NewQueue[*Slot](warmPoolSize),
		refillNeeded:  make(chan struct{}, 1),
		cniManager:    cniManager,
		hostInterface: gateway,
		slotConfig:    slotConfig,
		slotIDs:       slotstate.NewAllocator(firstSlotID, maxSlots),
	}

	return p, nil
}

func (p *Pool) createNetworkSlot(ctx context.Context) (*Slot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.slotIDs == nil {
		return nil, fmt.Errorf("network slot ID allocator is not initialized")
	}
	id, err := p.slotIDs.Acquire()
	if err != nil {
		return nil, fmt.Errorf("acquire network slot ID: %w", err)
	}
	slot, err := newSlot(id, p.slotConfig)
	if err != nil {
		releaseErr := p.slotIDs.Release(id)
		return nil, errors.Join(fmt.Errorf("construct allocated network slot: %w", err), releaseErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, p.slotIDs.Release(id))
	}

	handleCreationFailure := func(cause error) error {
		if cleanupErr := p.destroyNetworkSlot(context.Background(), slot); cleanupErr != nil {
			return errors.Join(
				cause,
				fmt.Errorf("clean up network slot %d: %w", slot.ID(), cleanupErr),
			)
		}
		return cause
	}

	if err := createNetworkNamespace(slot); err != nil {
		return nil, handleCreationFailure(fmt.Errorf("create network namespace for slot ID %d: %w", slot.ID(), err))
	}
	if err := p.provisionSlotNetwork(ctx, slot); err != nil {
		return nil, handleCreationFailure(fmt.Errorf("set up network for slot ID %d: %w", slot.ID(), err))
	}
	return slot, nil
}

func (p *Pool) destroyNetworkSlot(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}
	if err := p.teardownSlotNetwork(ctx, slot); err != nil {
		return fmt.Errorf("teardown network slot: %w", err)
	}
	if p.slotIDs == nil {
		return fmt.Errorf("network slot ID allocator is not initialized")
	}
	if err := p.slotIDs.Release(slot.ID()); err != nil {
		return fmt.Errorf("release network slot ID %d: %w", slot.ID(), err)
	}
	return nil
}

func (p *Pool) signalRefillNeeded() {
	if p == nil || p.refillNeeded == nil {
		return
	}
	select {
	case p.refillNeeded <- struct{}{}:
	default:
	}
}

// Start launches the pool's population loop. It must be called exactly once.

func (p *Pool) Start(ctx context.Context) error {
	if err := p.prefill(ctx); err != nil {
		return err
	}
	populateCtx, populateCancel := context.WithCancel(ctx)
	populateDone := make(chan struct{})
	p.populateCancel = populateCancel
	p.populateDone = populateDone
	go func() {
		defer close(populateDone)
		p.populate(populateCtx)
	}()
	return nil
}

// Close stops the population loop, drains and closes the warm queue, and makes
// a best-effort attempt to tear down every buffered slot.
func (p *Pool) Close() {
	defer ulog.TraceCost(ulog.TraceStart(), "", "netstack.Pool.Close()")
	if p == nil {
		return
	}
	if p.populateCancel != nil && p.populateDone != nil {
		p.populateCancel()
		<-p.populateDone
	}
	if p.warmSlots != nil {
		// Detach all buffered slots before starting cleanup so the queue is no
		// longer usable while teardown runs concurrently.
		var slots []*Slot
		for {
			slot, err := p.warmSlots.Pop()
			if err != nil {
				break
			}
			slots = append(slots, slot)
		}
		p.warmSlots.Close()

		if len(slots) > 0 {
			workers := cleanupWorkers
			if len(slots) < workers {
				workers = len(slots)
			}
			// Semaphore-bounded goroutines: slots are already materialized in
			// a slice, so a channel-based job queue would just re-buffer them.
			// The semaphore caps concurrency without that extra hop.
			sem := make(chan struct{}, workers)
			var wg sync.WaitGroup
			for _, slot := range slots {
				wg.Add(1)
				sem <- struct{}{}
				go func(s *Slot) {
					defer wg.Done()
					defer func() { <-sem }()
					if err := p.destroyNetworkSlot(context.Background(), s); err != nil {
						ulog.GetLogger().Warn(
							"failed to clean up warm network slot during shutdown",
							ulog.F("slot_id", s.ID()),
							ulog.F("error", err),
						)
					}
				}(slot)
			}
			wg.Wait()
		}
	}
	if p.cniManager != nil {
		if err := removeHostForwardingRules(p.cniManager.bridgeName, p.hostInterface); err != nil {
			ulog.GetLogger().Warn("failed to remove host forwarding rules during shutdown", ulog.F("bridge", p.cniManager.bridgeName), ulog.F("error", err))
		}
	}
}

// CleanupStaleResources removes network slots left behind by a previous
// conchd process. The allocator and warm queue are process-local, so an old
// slot cannot be returned to the new pool and must be torn down first.
func (p *Pool) CleanupStaleResources(ctx context.Context) error {
	if p == nil || p.cniManager == nil {
		return fmt.Errorf("network pool is not initialized")
	}

	entries, err := os.ReadDir(networkNamespaceDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("list stale network namespaces: %w", err)
		}
		entries = nil
	}

	var errs []error
	foundStaleSlot := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), networkNamespacePrefix) {
			continue
		}
		foundStaleSlot = true
		id, parseErr := strconv.Atoi(strings.TrimPrefix(entry.Name(), networkNamespacePrefix))
		if parseErr != nil {
			errs = append(errs, fmt.Errorf("invalid stale network namespace %s: %w", entry.Name(), parseErr))
			continue
		}
		slot, slotErr := newSlot(id, p.slotConfig)
		if slotErr != nil {
			errs = append(errs, slotErr)
			continue
		}
		if teardownErr := p.teardownSlotNetwork(ctx, slot); teardownErr != nil {
			errs = append(errs, fmt.Errorf("clean stale network slot %d: %w", id, teardownErr))
		}
	}
	staleCacheCount := 0
	if len(errs) == 0 {
		staleCacheCount, err = p.cniManager.reconcileStaleCache(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("reconcile stale CNI result cache: %w", err))
		} else if staleCacheCount > 0 {
			ulog.GetLogger().Info("cleaned stale CNI result cache", ulog.F("slot_count", staleCacheCount))
		}
	}

	// A bridge can be left after all stale slots are removed. Do not touch it
	// when no stale slot was found, and do not delete it after a failed CNI DEL.
	if (foundStaleSlot || staleCacheCount > 0) && len(errs) == 0 {
		if link, linkErr := netlink.LinkByName(p.cniManager.bridgeName); linkErr == nil {
			if deleteErr := netlink.LinkDel(link); deleteErr != nil {
				errs = append(errs, fmt.Errorf("delete stale Conch bridge: %w", deleteErr))
			}
		}
	}

	return errors.Join(errs...)
}

func (p *Pool) populate(ctx context.Context) {
	retryDelay := populateRetryMinDelay
	for {
		if p.warmSlots.IsClosed() {
			return
		}
		warmSlots, warmPoolSize := p.warmSlots.Usage()
		if warmSlots == warmPoolSize {
			select {
			case <-ctx.Done():
				return
			case <-p.refillNeeded:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
			slot, err := p.createNetworkSlot(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				ulog.GetLogger().Warn(
					"pool: failed to refill warm network slots; retrying",
					ulog.F("error", err),
					ulog.F("retry_delay", retryDelay),
					ulog.F("warm_slots", warmSlots),
					ulog.F("warm_pool_size", warmPoolSize),
				)
				if !p.waitForPopulateRetry(ctx, retryDelay) {
					return
				}
				retryDelay = min(retryDelay*2, populateRetryMaxDelay)
				continue
			}
			retryDelay = populateRetryMinDelay
			enqueueErr := p.warmSlots.Push(slot)
			if enqueueErr != nil {
				if destroyErr := p.destroyNetworkSlot(context.Background(), slot); destroyErr != nil {
					ulog.GetLogger().Warn("pool: failed to destroy unqueued warm slot", ulog.F("slot_id", slot.ID()), ulog.F("error", destroyErr))
				}
			}
			if errors.Is(enqueueErr, errWarmPoolClosed) || ctx.Err() != nil {
				return
			}
		}
	}
}

func (p *Pool) waitForPopulateRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-p.refillNeeded:
		return true
	case <-timer.C:
		return true
	}
}

func createNetworkSlotWithRetry(ctx context.Context, create func(context.Context) (*Slot, error)) (*Slot, error) {
	var slot *Slot
	var err error
	for attempt := 0; attempt < prefillCreateAttempts; attempt++ {
		slot, err = create(ctx)
		if err == nil || ctx.Err() != nil {
			return slot, err
		}
	}
	return slot, err
}

func (p *Pool) prefill(ctx context.Context) error {
	_, target := p.warmSlots.Usage()

	type job struct{}
	type result struct {
		slot *Slot
		err  error
	}

	// jobs fan out slot-creation work; results fan in worker outputs.
	jobs := make(chan job, target)
	results := make(chan result, target)

	workers := prefillWorkers
	if target < workers {
		workers = target
	}

	var workerWg sync.WaitGroup
	// Start a bounded worker pool to avoid overwhelming netns/iptables operations.
	for w := 0; w < workers; w++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for range jobs {
				if ctx.Err() != nil {
					return
				}
				slot, err := createNetworkSlotWithRetry(ctx, p.createNetworkSlot)
				// The results buffer holds every submitted job, so ownership can
				// always be transferred to the collector even after cancellation.
				results <- result{slot: slot, err: err}
			}
		}()
	}
	go func() {
		workerWg.Wait()
		close(results)
	}()

	var stopErr error

	// Submit one task per slot for the bounded-concurrency initial prefill.
submitJobs:
	for range target {
		select {
		case <-ctx.Done():
			stopErr = ctx.Err()
			break submitJobs
		case jobs <- job{}:
		}
	}
	close(jobs)

	var firstErr error
	for r := range results {
		if stopErr == nil {
			stopErr = ctx.Err()
		}
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}

		if stopErr == nil {
			enqueueErr := p.warmSlots.Push(r.slot)
			if enqueueErr == nil {
				continue
			}
			if errors.Is(enqueueErr, errWarmPoolClosed) {
				stopErr = errWarmPoolClosed
			} else if firstErr == nil {
				firstErr = enqueueErr
			}
		}

		firstErr = errors.Join(firstErr, p.destroyNetworkSlot(context.Background(), r.slot))
	}
	if stopErr != nil {
		return stopErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	inPool, _ := p.warmSlots.Usage()
	if firstErr != nil {
		return fmt.Errorf(
			"initial prefill stopped before reaching target: current=%d target=%d: %w",
			inPool, target, firstErr,
		)
	}

	ulog.GetLogger().Info("pool: initial prefill completed", ulog.F("acquired_total", inPool), ulog.F("in_pool", inPool), ulog.F("target", target))
	return nil
}

// Get acquires a warm network slot for a sandbox and applies its initial
// ingress and egress policy before returning the slot to the caller.
func (p *Pool) Get(ctx context.Context, sandboxID string, policy *SandboxNetworkConfig) (*Slot, error) {
	if err := ValidateSandboxNetworkInputConfig(ctx, policy); err != nil {
		return nil, err
	}
	slot, err := p.get(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	if slot == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("failed to apply initial sandbox network policy: %w", err),
			p.Discard(context.WithoutCancel(ctx), slot),
		)
	}
	if isNetworkConfigNonEmpty(policy) {
		if err := writeSandboxNetworkPolicyRules(ctx, slot, policy); err != nil {
			return nil, errors.Join(
				fmt.Errorf("failed to apply initial sandbox network policy: %w", err),
				p.Discard(context.WithoutCancel(ctx), slot),
			)
		}
	}
	return slot, nil
}

// get only acquires and assigns a warm slot. Initial policy setup is kept in
// Get so callers never receive a slot before its requested policy is applied.
func (p *Pool) get(ctx context.Context, sandboxID string) (*Slot, error) {
	if sandboxID == "" {
		return nil, fmt.Errorf("sandboxID is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s, err := p.warmSlots.Pop()
	if errors.Is(err, errWarmPoolClosed) {
		return nil, err
	}
	if errors.Is(err, errWarmPoolEmpty) {
		p.signalRefillNeeded()
		available, capacity := p.warmSlots.Usage()
		ulog.GetLogger().Warn(
			"no available network slot in the pool",
			ulog.F("capacity", capacity),
			ulog.F("available", available),
		)
		return nil, fmt.Errorf("no available network slot in the pool, warm_pool_size=%d: %w", capacity, err)
	}
	if err != nil {
		return nil, err
	}

	p.signalRefillNeeded()
	if s == nil {
		return nil, nil
	}
	s.assignSandbox(sandboxID)
	return s, nil
}

func (p *Pool) SetSandboxNetworkPolicy(ctx context.Context, slot *Slot, sandboxID string, policy *SandboxNetworkConfig) error {
	if p == nil {
		return fmt.Errorf("pool is nil")
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	if strings.TrimSpace(sandboxID) == "" || slot.sandboxID != sandboxID {
		return fmt.Errorf("network slot is not assigned to sandbox %q", sandboxID)
	}
	if err := ValidateSandboxNetworkInputConfig(ctx, policy); err != nil {
		return err
	}
	return writeSandboxNetworkPolicyRules(ctx, slot, policy)
}

func (p *Pool) provisionSlotNetwork(ctx context.Context, slot *Slot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || p.cniManager == nil {
		return fmt.Errorf("cni config not initialized")
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	netnsPath := slot.NetNSPath()
	cniID := slot.cniContainerID()

	cniResult, err := p.cniManager.SetupSandboxNetwork(ctx, cniID, netnsPath)
	if err != nil {
		return fmt.Errorf("failed to setup cni network: %w", err)
	}
	slot.recordCNIResult(cniResult)

	if err := runInNetNSPath(ctx, netnsPath, func() error {
		return configureGuestTapNetwork(slot, cniResult.IP)
	}); err != nil {
		return fmt.Errorf("failed to setup guest tap network: %w", err)
	}
	if err := runInNetNSPath(ctx, netnsPath, func() error {
		addresses, err := netlink.AddrList(nil, netlink.FAMILY_V6)
		if err != nil {
			return err
		}
		if len(addresses) != 0 {
			return fmt.Errorf("network namespace has IPv6 address %s", addresses[0].String())
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		if len(routes) != 0 {
			return fmt.Errorf("network namespace has IPv6 route %+v", routes[0])
		}
		return configureIPv4OnlyCurrentNetworkNamespace()
	}); err != nil {
		return fmt.Errorf("enforce IPv4-only network namespace: %w", err)
	}

	return nil
}

func (p *Pool) Release(ctx context.Context, slot *Slot) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		if slot != nil {
			cleanupCtx := context.WithoutCancel(ctx)
			if err := prepareSlotForReuse(cleanupCtx, slot); err != nil {
				discardErr := p.Discard(cleanupCtx, slot)
				return errors.Join(err, discardErr)
			}
			slotHealthErr := p.slotHealth(ctx, slot)
			if slotHealthErr == nil {
				if err := p.warmSlots.Push(slot); err != nil {
					if destroyErr := p.destroyNetworkSlot(context.Background(), slot); destroyErr != nil {
						return errors.Join(
							fmt.Errorf("failed to enqueue released network slot: %w", err),
							fmt.Errorf("failed to destroy unqueued released network slot: %w", destroyErr),
						)
					}
					ulog.GetLogger().Info("discarded released slot because it could not return to the warm pool", ulog.F("slot_id", slot.ID()), ulog.F("reason", err))
					return nil
				}
				ulog.GetLogger().Info("slot released back to pool", ulog.F("slot_id", slot.ID()))
			} else {
				ulog.GetLogger().Warn("slot unhealthy, dropping from the pool", ulog.F("slot_id", slot.ID()), ulog.F("error", slotHealthErr))
				if err := p.Discard(ctx, slot); err != nil {
					return fmt.Errorf("failed to discard unhealthy network slot %d: %w", slot.ID(), err)
				}
			}
		}
		return nil
	}
}

func prepareSlotForReuse(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}
	if err := clearSandboxNetworkPolicyRules(ctx, slot); err != nil {
		return fmt.Errorf("failed to clear sandbox network policy: %w", err)
	}
	if err := FlushSandboxConntrack(ctx, slot); err != nil {
		return fmt.Errorf("failed to flush sandbox conntrack: %w", err)
	}
	slot.clearSandboxAssignment()
	return nil
}

func (p *Pool) Discard(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}
	if err := p.destroyNetworkSlot(ctx, slot); err != nil {
		ulog.GetLogger().Error("failed to discard network slot", ulog.F("slot_id", slot.ID()), ulog.F("error", err))
		return fmt.Errorf("failed to discard network slot %d: %w", slot.ID(), err)
	}
	p.signalRefillNeeded()
	return nil
}

func (p *Pool) teardownSlotNetwork(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}

	var errs []error
	netnsPath := slot.NetNSPath()
	netnsMounted := isNetworkNamespaceMounted(netnsPath)
	if cniIP := slot.CNIIP(); cniIP != "" {
		if netnsMounted {
			if err := runInNetNSPath(ctx, netnsPath, func() error {
				return removeGuestTapNetwork(slot, cniIP)
			}); err != nil {
				errs = append(errs, err)
			}
		}
	}

	var cniErr error
	cniTeardownComplete := false
	if p == nil || p.cniManager == nil || p.cniManager.backend == nil {
		cniErr = fmt.Errorf("cni config not initialized")
	} else {
		cniNetNSPath := netnsPath
		if !netnsMounted {
			cniNetNSPath = ""
		}
		cniErr = p.cniManager.TeardownSandboxNetwork(ctx, slot.cniContainerID(), cniNetNSPath)
	}
	if cniErr == nil {
		slot.clearCNIResult()
		cniTeardownComplete = true
	} else {
		errs = append(errs, cniErr)
	}
	slot.clearSandboxAssignment()
	if cniTeardownComplete {
		if err := deleteNetworkNamespace(slot); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
func (p *Pool) slotHealth(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	if _, err := os.Stat(slot.NetNSPath()); err != nil {
		return fmt.Errorf("namespace missing: %w", err)
	}
	if slot.sandboxID != "" {
		return fmt.Errorf("slot is still assigned to sandbox %s", slot.sandboxID)
	}
	if slot.CNIIP() == "" {
		return fmt.Errorf("slot has no cni IP")
	}
	if p == nil || p.cniManager == nil || p.cniManager.backend == nil {
		return fmt.Errorf("cni config not initialized")
	}
	if err := p.cniManager.checkSandboxInterface(ctx, slot.NetNSPath(), slot.CNIIP()); err != nil {
		return err
	}
	if err := checkGuestTapNetwork(ctx, slot); err != nil {
		return err
	}
	return nil
}
