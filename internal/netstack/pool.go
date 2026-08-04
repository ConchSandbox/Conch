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
	"strings"
	"sync"
	"time"

	slotstate "github.com/openeuler/Conch/internal/netstack/slot"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	DefaultWarmPoolSize   = 250
	maxSlots              = 4000
	prefillWorkers        = 16
	populateRetryMinDelay = time.Second
	populateRetryMaxDelay = 30 * time.Second
)

var (
	errWarmPoolClosed = slotstate.ErrClosed
	errWarmPoolEmpty  = slotstate.ErrEmpty
)

func getLogger() ulog.Logger {
	return ulog.GetLogger()
}

type Pool struct {
	warmSlots          *slotstate.Queue[*Slot]
	warmSlotNeeded     chan struct{}
	populateCancel     context.CancelFunc
	populateDone       <-chan struct{}
	dynamicReservation bool
	cniManager         *CNIManager
	slotIDs            *slotstate.Allocator
	prefillReady       chan struct{}
	prefillReadyOnce   sync.Once
}

func normalizeAndValidateWarmPoolSize(warmPoolSize int) (int, error) {
	if warmPoolSize == 0 {
		warmPoolSize = DefaultWarmPoolSize
	}
	if warmPoolSize < 1 {
		return 0, fmt.Errorf("invalid network.warm_pool_size=%d, must be positive", warmPoolSize)
	}
	if warmPoolSize > maxSlots {
		return 0, fmt.Errorf("invalid network.warm_pool_size=%d, exceeds maximum supported slots=%d", warmPoolSize, maxSlots)
	}
	return warmPoolSize, nil
}

func NewPool(warmPoolSize int, dynamicReservation bool, tapIP string, tapMask int, cniCfg CNIManagerConfig) (*Pool, error) {
	warmPoolSize, err := normalizeAndValidateWarmPoolSize(warmPoolSize)
	if err != nil {
		return nil, err
	}
	if err := configureTapNetwork(tapIP, tapMask); err != nil {
		return nil, fmt.Errorf("invalid tap network config: %w", err)
	}
	if err := prepareNetworkNamespaceDirectory(); err != nil {
		return nil, fmt.Errorf("prepare network namespace directory: %w", err)
	}
	cniManager, err := NewCNIManager(cniCfg)
	if err != nil {
		return nil, err
	}

	p := &Pool{
		warmSlots:          slotstate.NewQueue[*Slot](warmPoolSize),
		warmSlotNeeded:     make(chan struct{}, 1),
		dynamicReservation: dynamicReservation,
		cniManager:         cniManager,
		slotIDs:            slotstate.NewAllocator(firstSlotID, maxSlots),
		prefillReady:       make(chan struct{}),
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
	slot, err := NewSlot(id)
	if err != nil {
		releaseErr := p.slotIDs.Release(id)
		return nil, errors.Join(fmt.Errorf("construct allocated network slot: %w", err), releaseErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, p.slotIDs.Release(id))
	}

	handleCreationFailure := func(cause error) error {
		if cleanupErr := p.cleanupSlotAllocation(slot); cleanupErr != nil {
			return errors.Join(
				cause,
				fmt.Errorf("clean up network slot %d: %w", slot.ID, cleanupErr),
			)
		}
		return cause
	}

	if err := p.initializeNetworkSlot(ctx, slot); err != nil {
		return nil, handleCreationFailure(err)
	}
	return slot, nil
}

func (p *Pool) initializeNetworkSlot(ctx context.Context, slot *Slot) error {
	if p == nil {
		return fmt.Errorf("pool is nil")
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	if err := createNetworkNamespace(slot.ID); err != nil {
		return fmt.Errorf("create network namespace for slot ID %d: %w", slot.ID, err)
	}
	if err := p.setupSlotNetwork(ctx, slot); err != nil {
		return fmt.Errorf("set up network for slot ID %d: %w", slot.ID, err)
	}
	return nil
}

func (p *Pool) cleanupSlotAllocation(slot *Slot) error {
	if slot == nil {
		return nil
	}
	if err := p.teardownSlotNetwork(context.Background(), slot); err != nil {
		return fmt.Errorf("teardown network slot: %w", err)
	}
	return p.releaseSlotID(slot)
}

func (p *Pool) releaseSlotID(slot *Slot) error {
	if p == nil || slot == nil {
		return nil
	}
	if p.slotIDs == nil {
		return fmt.Errorf("network slot ID allocator is not initialized")
	}
	if err := p.slotIDs.Release(slot.ID); err != nil {
		return fmt.Errorf("release network slot ID %d: %w", slot.ID, err)
	}
	return nil
}

// Start launches the pool's population loop. It must be called exactly once.
func (p *Pool) Start(ctx context.Context) {
	populateCtx, populateCancel := context.WithCancel(ctx)
	populateDone := make(chan struct{})
	p.populateCancel = populateCancel
	p.populateDone = populateDone
	go func() {
		defer close(populateDone)
		p.populate(populateCtx)
	}()
}

// Close stops the population loop, drains and closes the warm queue, and makes
// a best-effort attempt to tear down every buffered slot.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	if p.populateCancel != nil && p.populateDone != nil {
		p.populateCancel()
		<-p.populateDone
	}
	if p.warmSlots != nil {
		for {
			slot, err := p.warmSlots.Pop()
			if err != nil {
				break
			}
			if err := p.discardSlotWithoutReplacement(slot); err != nil {
				getLogger().Warn("failed to clean up warm network slot during shutdown", ulog.F("slot_id", slot.ID), ulog.F("error", err))
			}
		}
		p.warmSlots.Close()
	}
}

func (p *Pool) populate(ctx context.Context) {
	if !p.dynamicReservation {
		if err := p.populateStatic(ctx); err != nil && !errors.Is(err, errWarmPoolClosed) {
			getLogger().Warn("pool: static reservation exited with error", ulog.F("error", err))
		}
		p.prefillReadyOnce.Do(func() {
			close(p.prefillReady)
		})
		<-ctx.Done()
		return
	}

	retryDelay := populateRetryMinDelay
	for {
		warmSlots, warmPoolSize := p.warmSlots.Usage()
		if warmSlots >= warmPoolSize {
			if !p.waitForWarmSlotNeed(ctx) {
				return
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
				getLogger().Warn(
					"pool: failed to replenish warm network slots; retrying",
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
				if discardErr := p.discardSlotWithoutReplacement(slot); discardErr != nil {
					getLogger().Warn("pool: failed to discard unqueued warm slot", ulog.F("slot_id", slot.ID), ulog.F("error", discardErr))
				}
			}
			if errors.Is(enqueueErr, errWarmPoolClosed) || ctx.Err() != nil {
				return
			}
		}
	}
}

func (p *Pool) waitForWarmSlotNeed(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-p.warmSlotNeeded:
		return true
	}
}

func (p *Pool) signalWarmSlotNeeded() {
	if p.warmSlotNeeded == nil {
		return
	}
	select {
	case p.warmSlotNeeded <- struct{}{}:
	default:
	}
}

func (p *Pool) waitForPopulateRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (p *Pool) discardSlotWithoutReplacement(slot *Slot) error {
	if slot == nil {
		return nil
	}
	if err := p.cleanupSlotAllocation(slot); err != nil {
		getLogger().Warn("failed to discard unqueued network slot", ulog.F("slot_id", slot.ID), ulog.F("error", err))
		return err
	}
	return nil
}

func (p *Pool) handleCreatedSlotAfterCancel(slot *Slot) {
	if err := p.discardSlotWithoutReplacement(slot); err != nil {
		getLogger().Warn("failed to clean up network slot after population stopped", ulog.F("slot_id", slot.ID), ulog.F("error", err))
	}
}

func (p *Pool) populateStatic(ctx context.Context) error {
	if p.warmSlots.IsClosed() {
		return errWarmPoolClosed
	}
	inPool, warmPoolSize := p.warmSlots.Usage()
	target := warmPoolSize - inPool
	if target <= 0 {
		getLogger().Info("pool: static reservation completed", ulog.F("acquired_total", inPool), ulog.F("in_pool", inPool), ulog.F("target", warmPoolSize))
		return nil
	}

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
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-jobs:
					if !ok {
						return
					}
					slot, err := p.createNetworkSlot(ctx)

					select {
					case <-ctx.Done():
						p.handleCreatedSlotAfterCancel(slot)
						return
					case results <- result{slot: slot, err: err}:
					}
				}
			}
		}()
	}
	go func() {
		workerWg.Wait()
		close(results)
	}()

	// Submit one prefill task per target slot; static reservation is one-shot.
	for i := 0; i < target; i++ {
		select {
		case <-ctx.Done():
			close(jobs)
			for r := range results {
				if r.err == nil {
					p.handleCreatedSlotAfterCancel(r.slot)
				}
			}
			return ctx.Err()
		case jobs <- job{}:
		}
	}
	close(jobs)

	var firstErr error
	canceled := false
	queueClosed := false
	for r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		if queueClosed {
			if err := p.discardSlotWithoutReplacement(r.slot); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if canceled {
			p.handleCreatedSlotAfterCancel(r.slot)
			continue
		}
		enqueueErr := p.warmSlots.Push(r.slot)
		if enqueueErr != nil {
			if errors.Is(enqueueErr, errWarmPoolClosed) {
				queueClosed = true
			}
			if discardErr := p.discardSlotWithoutReplacement(r.slot); discardErr != nil && firstErr == nil {
				firstErr = discardErr
			}
		}
		if ctx.Err() != nil {
			canceled = true
		}
	}
	if queueClosed {
		return errWarmPoolClosed
	}
	if canceled {
		return ctx.Err()
	}

	inPool, _ = p.warmSlots.Usage()
	if firstErr != nil {
		return fmt.Errorf(
			"static reservation stopped before reaching target: current=%d target=%d: %w",
			inPool, target, firstErr,
		)
	}

	getLogger().Info("pool: static reservation completed", ulog.F("acquired_total", inPool), ulog.F("in_pool", inPool), ulog.F("target", warmPoolSize))
	return nil
}

func (p *Pool) Get(ctx context.Context, sandboxID string) (*Slot, error) {
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
		available, capacity := p.warmSlots.Usage()
		getLogger().Warn(
			"no available network slot in the pool",
			ulog.F("capacity", capacity),
			ulog.F("available", available),
			ulog.F("prefill_ready", p.isPrefillReady()),
			ulog.F("dynamic_reservation", p.dynamicReservation),
		)
		return nil, fmt.Errorf("no available network slot in the pool, warm_pool_size=%d: %w", capacity, err)
	}
	if err != nil {
		return nil, err
	}

	p.signalWarmSlotNeeded()
	if s == nil {
		return nil, nil
	}
	s.assignSandbox(sandboxID)
	return s, nil
}

func (p *Pool) isPrefillReady() bool {
	select {
	case <-p.prefillReady:
		return true
	default:
		return false
	}
}

func (p *Pool) setupSlotNetwork(ctx context.Context, slot *Slot) error {
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
	if _, _, err := p.cniManager.SelectCNIPluginAndConfig(slot); err != nil {
		return err
	}
	cniID := slot.CNIContainerID()
	opts, err := buildCNIOpts(slot, cniID, netnsPath)
	if err != nil {
		return err
	}

	cniResult, err := p.cniManager.SetupSandboxNetwork(ctx, cniID, netnsPath, opts...)
	if err != nil {
		return fmt.Errorf("failed to setup cni network: %w", err)
	}
	slot.setCNIResult(cniResult)

	if err := SetupGuestTapNetwork(ctx, slot, netnsPath, cniResult); err != nil {
		return fmt.Errorf("failed to setup guest tap network: %w", err)
	}

	return nil
}

func (p *Pool) checkSlotCNI(ctx context.Context, slot *Slot) error {
	if p == nil || p.cniManager == nil {
		return fmt.Errorf("cni config not initialized")
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	cniID := slot.CNIContainerID()
	netnsPath := slot.NetNSPath()
	opts, err := buildCNIOpts(slot, cniID, netnsPath)
	if err != nil {
		return err
	}
	return p.cniManager.CheckSandboxNetwork(ctx, cniID, netnsPath, opts...)
}

func (p *Pool) Release(ctx context.Context, slot *Slot) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		if slot != nil {
			slot.clearSandboxAssignment()
			slotHealthErr := p.slotHealth(ctx, slot)
			if slotHealthErr == nil {
				if err := p.warmSlots.Push(slot); err != nil {
					if discardErr := p.discardSlotWithoutReplacement(slot); discardErr != nil {
						return errors.Join(
							fmt.Errorf("failed to enqueue released network slot: %w", err),
							fmt.Errorf("failed to discard unqueued released network slot: %w", discardErr),
						)
					}
					getLogger().Info("discarded released slot because it could not return to the warm pool", ulog.F("slot_id", slot.ID), ulog.F("reason", err))
					return nil
				}
				getLogger().Info("slot released back to pool", ulog.F("slot_id", slot.ID))
			} else {
				getLogger().Warn("slot unhealthy, dropping from the pool", ulog.F("slot_id", slot.ID), ulog.F("error", slotHealthErr))
				if err := p.Discard(ctx, slot); err != nil {
					return fmt.Errorf("failed to discard unhealthy network slot %d: %w", slot.ID, err)
				}
			}
		}
		return nil
	}
}

func (p *Pool) Discard(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}

	if err := p.teardownSlotNetwork(ctx, slot); err != nil {
		getLogger().Error("failed to discard network slot", ulog.F("slot_id", slot.ID), ulog.F("error", err))
		return fmt.Errorf("failed to discard network slot %d: %w", slot.ID, err)
	}

	if err := p.releaseSlotID(slot); err != nil {
		return err
	}

	if err := p.replenishDroppedSlot(ctx); err != nil {
		getLogger().Warn("failed to replenish discarded network slot", ulog.F("slot_id", slot.ID), ulog.F("error", err))
		return err
	}
	return nil
}

func (p *Pool) replenishDroppedSlot(ctx context.Context) error {
	if p.warmSlots.IsClosed() {
		return nil
	}

	slot, err := p.createNetworkSlot(ctx)
	if err != nil {
		return fmt.Errorf("failed to create replacement network slot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		if discardErr := p.discardSlotWithoutReplacement(slot); discardErr != nil {
			return errors.Join(err, fmt.Errorf("failed to discard canceled replacement network slot: %w", discardErr))
		}
		return err
	}
	if err := p.warmSlots.Push(slot); err == nil {
		return nil
	}
	if discardErr := p.discardSlotWithoutReplacement(slot); discardErr != nil {
		return fmt.Errorf("failed to discard unqueued replacement network slot: %w", discardErr)
	}
	return nil
}

func (p *Pool) teardownSlotNetwork(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}

	var errs []error
	netnsPath := slot.NetNSPath()
	if result := slot.CNIResult(); result != nil {
		if cniNetworkNamespacePath(netnsPath) != "" {
			if err := TeardownGuestTapNetwork(ctx, slot, netnsPath, result); err != nil {
				errs = append(errs, err)
			}
		}
	}

	var cniErr error
	cniTeardownComplete := false
	if p == nil || p.cniManager == nil {
		cniErr = fmt.Errorf("cni config not initialized")
	} else {
		cniErr = p.teardownSandboxNetworkWithRetry(ctx, slot, netnsPath)
	}
	if cniErr == nil {
		slot.clearCNIResult()
		cniTeardownComplete = true
	} else {
		errs = append(errs, cniErr)
	}
	slot.clearSandboxAssignment()
	if cniTeardownComplete {
		if err := DeleteSandboxNetworkNamespace(slot.ID); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (p *Pool) teardownSandboxNetworkWithRetry(ctx context.Context, slot *Slot, netnsPath string) error {
	cniID := slot.CNIContainerID()
	opts, err := buildCNIOpts(slot, cniID, netnsPath)
	if err != nil {
		return err
	}
	cniNetNSPath := cniNetworkNamespacePath(netnsPath)
	var lastErr error
	for attempt := 0; attempt <= cniTeardownRetryAttempts; attempt++ {
		err := p.cniManager.TeardownSandboxNetwork(ctx, cniID, cniNetNSPath, opts...)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isCNIBusyTeardownError(err) || attempt == cniTeardownRetryAttempts {
			return err
		}
		delay := cniTeardownRetryDelay * time.Duration(1<<attempt)
		select {
		case <-ctx.Done():
			return errors.Join(lastErr, ctx.Err())
		case <-time.After(delay):
		}
	}
	return lastErr
}

func isCNIBusyTeardownError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "resource busy")
}

func (p *Pool) slotHealth(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	if _, err := os.Stat(slot.NetNSPath()); err != nil {
		return fmt.Errorf("namespace missing: %w", err)
	}
	if slot.SandboxID() != "" {
		return fmt.Errorf("slot is still assigned to sandbox %s", slot.SandboxID())
	}
	if slot.CNIResult() == nil || slot.CNIResult().IP == "" {
		return fmt.Errorf("slot has no cni result")
	}
	if p == nil || p.cniManager == nil {
		return fmt.Errorf("cni config not initialized")
	}
	if err := ValidateReusableSlotNetwork(ctx, slot, slot.NetNSPath(), p.cniManager.config.IfName); err != nil {
		return err
	}
	if err := p.checkSlotCNI(ctx, slot); err != nil {
		return err
	}
	return nil
}
