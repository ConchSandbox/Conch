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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	DefaultWarmPoolSize     = 250
	maxSlots                = 4000
	prefillWorkers          = 16
	populateRetryMinDelay   = time.Second
	populateRetryMaxDelay   = 30 * time.Second
	cniBridgeCleanupRetries = 300
	cniBridgeCleanupDelay   = 100 * time.Millisecond
)

var (
	ErrNetworkSlotStoreRead = errors.New("network slot store read failed")
	ErrNetworkSlotCleanup   = errors.New("network slot cleanup failed")
	ErrNetworkSlotCapacity  = errors.New("network slot capacity is below persisted usage")
	errPoolCleanupRequested = errors.New("network pool cleanup requested")
	errWarmPoolFull         = errors.New("warm network pool is already full")
)

func getLogger() ulog.Logger {
	return ulog.GetLogger()
}

type NetworkSlotStore interface {
	UpsertNetworkSlot(context.Context, state.NetworkSlotRecord) error
	ListNetworkSlots(context.Context) ([]state.NetworkSlotRecord, error)
	DeleteNetworkSlot(context.Context, string) error
}

type preserveOnCancelContextKey struct{}

func WithPreserveOnCancel(ctx context.Context) context.Context {
	return context.WithValue(ctx, preserveOnCancelContextKey{}, true)
}

type Pool struct {
	slotStorage        Storage
	newSlots           chan *Slot
	warmSlotNeeded     chan struct{}
	done               chan struct{}
	dynamicReservation bool
	cniManager         *CNIManager
	slotStore          NetworkSlotStore
	inUse              map[string]*Slot
	inUseMu            sync.Mutex
	stopOnce           sync.Once
	prefillReady       chan struct{}
	prefillReadyOnce   sync.Once
	populateMu         sync.Mutex
	populateStarted    bool
	populateCtx        context.Context
	populateCancel     context.CancelCauseFunc
	populateDone       chan struct{}
	slotHealthCheck    func(context.Context, *Slot) error
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

func NewPool(warmPoolSize int, dynamicReservation bool, tapIP string, tapMask int, cniCfg CNIManagerConfig, slotStore NetworkSlotStore) (*Pool, error) {
	warmPoolSize, err := normalizeAndValidateWarmPoolSize(warmPoolSize)
	if err != nil {
		return nil, err
	}
	if err := configureTapNetwork(tapIP, tapMask); err != nil {
		return nil, fmt.Errorf("invalid tap network config: %w", err)
	}
	newSlots := make(chan *Slot, warmPoolSize)

	slotStorage, err := NewStorage(maxSlots)
	if err != nil {
		return nil, fmt.Errorf("failed to create new storage: %w", err)
	}

	cniManager, err := NewCNIManager(cniCfg)
	if err != nil {
		return nil, err
	}

	p := &Pool{
		slotStorage:        slotStorage,
		newSlots:           newSlots,
		warmSlotNeeded:     make(chan struct{}, 1),
		done:               make(chan struct{}),
		dynamicReservation: dynamicReservation,
		cniManager:         cniManager,
		slotStore:          slotStore,
		inUse:              make(map[string]*Slot),
		prefillReady:       make(chan struct{}),
		populateDone:       make(chan struct{}),
	}

	return p, nil
}

func (p *Pool) createNetworkSlot(ctx context.Context) (*Slot, error) {
	slot, err := p.slotStorage.Acquire(ctx)
	if err != nil {
		if isExpectedShutdownError(ctx, err) {
			return nil, context.Canceled
		}
		getLogger().Error("failed to acquire network slot", ulog.F("error", err))
		return nil, fmt.Errorf("failed to acquire network slot: %w", err)
	}
	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCreating, "", nil); err != nil {
		_ = p.slotStorage.Release(slot)
		return nil, fmt.Errorf("failed to record creating network slot: %w", err)
	}

	err = slot.CreateNetwork()
	if err != nil {
		if isExpectedShutdownError(ctx, err) {
			p.markSlotCleaning(context.Background(), slot, slot.SandboxID(), err)
			getLogger().Debug("network creation interrupted during shutdown", ulog.F("error", err))
			return nil, context.Canceled
		}
		cleanupErr := DeleteSandboxNetworkNamespace(slot.NetNSPath())
		if cleanupErr == nil {
			err = errors.Join(err, p.slotStorage.Release(slot), p.deleteSlotRecord(context.Background(), slot))
		} else {
			err = errors.Join(err, fmt.Errorf("failed to rollback namespace creation; slot left acquired: %w", cleanupErr))
			p.markSlotCleaning(context.Background(), slot, slot.SandboxID(), err)
		}
		getLogger().Error("failed to create network", ulog.F("error", err))
		return nil, fmt.Errorf("failed to create network: %w", err)
	}

	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCreating, "", nil); err != nil {
		rollbackErr := errors.Join(DeleteSandboxNetworkNamespace(slot.NetNSPath()), p.slotStorage.Release(slot), p.deleteSlotRecord(context.Background(), slot))
		if rollbackErr != nil {
			p.markSlotCleaning(context.Background(), slot, slot.SandboxID(), errors.Join(err, rollbackErr))
			return nil, errors.Join(
				fmt.Errorf("failed to record network namespace creation: %w", err),
				fmt.Errorf("failed to rollback unrecorded network namespace; slot left acquired: %w", rollbackErr),
			)
		}
		return nil, fmt.Errorf("failed to record network namespace creation: %w", err)
	}

	if err := p.setupSlotNetwork(ctx, slot); err != nil {
		if isExpectedShutdownError(ctx, err) {
			p.markSlotCleaning(context.Background(), slot, slot.SandboxID(), err)
			getLogger().Debug("network creation interrupted during shutdown", ulog.F("error", err))
			return nil, context.Canceled
		}
		teardownErr := p.teardownSlotNetwork(context.WithoutCancel(ctx), slot)
		if teardownErr == nil {
			err = errors.Join(err, p.slotStorage.Release(slot), p.deleteSlotRecord(context.Background(), slot))
		} else {
			err = errors.Join(err, fmt.Errorf("failed to rollback slot network setup; slot left acquired: %w", teardownErr))
			p.markSlotCleaning(context.Background(), slot, slot.SandboxID(), err)
		}
		getLogger().Error("failed to setup slot network", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
		return nil, fmt.Errorf("failed to setup slot network for slot index %d: %w", slot.Idx, err)
	}

	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotWarmIdle, "", nil); err != nil {
		rollbackErr := errors.Join(p.teardownSlotNetwork(context.WithoutCancel(ctx), slot), p.slotStorage.Release(slot), p.deleteSlotRecord(context.Background(), slot))
		if rollbackErr != nil {
			p.markSlotCleaning(context.Background(), slot, slot.SandboxID(), errors.Join(err, rollbackErr))
			return nil, errors.Join(
				fmt.Errorf("failed to record warm network slot: %w", err),
				fmt.Errorf("failed to rollback unrecorded warm network slot; slot left acquired: %w", rollbackErr),
			)
		}
		return nil, fmt.Errorf("failed to record warm network slot: %w", err)
	}
	return slot, nil
}

func shouldPreserveAfterCancel(ctx context.Context) bool {
	if ctx == nil || ctx.Err() == nil {
		return false
	}
	if errors.Is(context.Cause(ctx), errPoolCleanupRequested) {
		return false
	}
	preserve, _ := ctx.Value(preserveOnCancelContextKey{}).(bool)
	return preserve
}

func isExpectedShutdownError(ctx context.Context, err error) bool {
	// Without an error or an active context cancellation, this is not shutdown-related noise.
	if err == nil || !shouldPreserveAfterCancel(ctx) {
		return false
	}
	// Prefer typed context errors when the lower layers preserve them.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Some external command failures during Ctrl+C only surface as stderr text.
	msg := err.Error()
	return strings.Contains(msg, "exit status -1") || strings.Contains(msg, "signal: interrupt")
}

func (p *Pool) upsertSlotRecord(ctx context.Context, slot *Slot, slotState, sandboxID string, err error) error {
	if p == nil || p.slotStore == nil || slot == nil {
		return nil
	}
	rec := state.NetworkSlotRecord{
		SlotKey:   slot.Key,
		SlotIndex: slot.Idx,
		State:     slotState,
		SandboxID: sandboxID,
		NetNSPath: slot.NetNSPath(),
		UpdatedAt: time.Now().UnixNano(),
	}
	if result := slot.CNIResult(); result != nil {
		rec.CNIID = slot.CNIContainerID()
		rec.CNIIP = result.IP
	}
	if err != nil {
		rec.LastError = err.Error()
	}
	return p.slotStore.UpsertNetworkSlot(ctx, rec)
}

func (p *Pool) deleteSlotRecord(ctx context.Context, slot *Slot) error {
	if p == nil || p.slotStore == nil || slot == nil {
		return nil
	}
	return p.slotStore.DeleteNetworkSlot(ctx, slot.Key)
}

func (p *Pool) markSlotCleaning(ctx context.Context, slot *Slot, sandboxID string, err error) {
	if writeErr := p.upsertSlotRecord(ctx, slot, state.NetworkSlotCleaning, sandboxID, err); writeErr != nil {
		getLogger().Warn("failed to mark network slot cleaning", ulog.F("slot_index", slot.Idx), ulog.F("error", writeErr))
	}
}

func (p *Pool) Populate(ctx context.Context) {
	ctx, cancel := context.WithCancelCause(ctx)
	p.populateMu.Lock()
	if p.populateStarted {
		p.populateMu.Unlock()
		cancel(nil)
		return
	}
	if p.populateDone == nil {
		p.populateDone = make(chan struct{})
	}
	p.populateStarted = true
	p.populateCtx = ctx
	p.populateCancel = cancel
	populateDone := p.populateDone
	p.populateMu.Unlock()

	defer cancel(nil)
	defer close(populateDone)
	defer close(p.newSlots)

	if !p.dynamicReservation {
		if err := p.populateStatic(ctx); err != nil {
			getLogger().Warn("pool: static reservation exited with error", ulog.F("error", err))
		}
		p.prefillReadyOnce.Do(func() {
			close(p.prefillReady)
		})
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			return
		}
	}

	retryDelay := populateRetryMinDelay
	for {
		if len(p.newSlots) >= cap(p.newSlots) {
			if !p.waitForWarmSlotNeed(ctx) {
				return
			}
			continue
		}
		select {
		case <-p.done:
			return
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
					ulog.F("warm_slots", len(p.newSlots)),
					ulog.F("warm_pool_size", cap(p.newSlots)),
				)
				if !p.waitForPopulateRetry(ctx, retryDelay) {
					return
				}
				retryDelay = min(retryDelay*2, populateRetryMaxDelay)
				continue
			}
			retryDelay = populateRetryMinDelay
			select {
			case <-p.done:
				_ = p.discardCreatedSlot(slot)
				return
			case <-ctx.Done():
				p.handleCreatedSlotAfterCancel(ctx, slot)
				return
			case p.newSlots <- slot:
			default:
				if err := p.discardCreatedSlot(slot); err != nil {
					getLogger().Warn("pool: failed to discard excess warm slot", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
				}
			}
		}
	}
}

func (p *Pool) waitForWarmSlotNeed(ctx context.Context) bool {
	select {
	case <-p.done:
		return false
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
	case <-p.done:
		return false
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (p *Pool) discardCreatedSlot(slot *Slot) error {
	if slot == nil {
		return nil
	}
	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCleaning, "", nil); err != nil {
		getLogger().Warn("failed to record network slot cleanup", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
	}
	if err := p.teardownSlotNetwork(context.Background(), slot); err != nil {
		getLogger().Warn("failed to discard network slot during pool stop; slot left acquired", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
		p.markSlotCleaning(context.Background(), slot, slot.SandboxID(), err)
		return err
	}
	if err := p.slotStorage.Release(slot); err != nil {
		getLogger().Warn("failed to release discarded network slot during pool stop", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
		p.markSlotCleaning(context.Background(), slot, slot.SandboxID(), err)
		return err
	}
	return p.deleteSlotRecord(context.Background(), slot)
}

func (p *Pool) preserveCreatedSlot(slot *Slot) {
	if slot == nil {
		return
	}
	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotWarmIdle, "", nil); err != nil {
		getLogger().Warn("failed to preserve network slot record during shutdown", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
	}
}

func (p *Pool) handleCreatedSlotAfterCancel(ctx context.Context, slot *Slot) {
	if shouldPreserveAfterCancel(ctx) {
		p.preserveCreatedSlot(slot)
		return
	}
	_ = p.discardCreatedSlot(slot)
}

func (p *Pool) isStopping() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *Pool) populateStatic(ctx context.Context) error {
	target := cap(p.newSlots) - len(p.newSlots)
	if target <= 0 {
		getLogger().Info("pool: static reservation completed", ulog.F("acquired_total", len(p.newSlots)), ulog.F("in_pool", len(p.newSlots)), ulog.F("target", cap(p.newSlots)))
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
				case <-p.done:
					return
				case _, ok := <-jobs:
					if !ok {
						return
					}
					slot, err := p.createNetworkSlot(ctx)

					select {
					case <-ctx.Done():
						p.handleCreatedSlotAfterCancel(ctx, slot)
						return
					case <-p.done:
						_ = p.discardCreatedSlot(slot)
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
		case <-p.done:
			close(jobs)
			for r := range results {
				if r.err == nil {
					_ = p.discardCreatedSlot(r.slot)
				}
			}
			return nil
		case <-ctx.Done():
			close(jobs)
			for r := range results {
				if r.err == nil {
					p.handleCreatedSlotAfterCancel(ctx, r.slot)
				}
			}
			return ctx.Err()
		case jobs <- job{}:
		}
	}
	close(jobs)

	var firstErr error
	stopping := false
	preserving := false
	for r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		if stopping || p.isStopping() {
			stopping = true
			_ = p.discardCreatedSlot(r.slot)
			continue
		}
		if preserving || ctx.Err() != nil {
			preserving = true
			p.handleCreatedSlotAfterCancel(ctx, r.slot)
			continue
		}
		select {
		case <-p.done:
			stopping = true
			_ = p.discardCreatedSlot(r.slot)
		case <-ctx.Done():
			preserving = true
			p.handleCreatedSlotAfterCancel(ctx, r.slot)
		case p.newSlots <- r.slot:
		}
	}
	if stopping {
		return nil
	}
	if preserving {
		return ctx.Err()
	}

	if firstErr != nil {
		return fmt.Errorf(
			"static reservation stopped before reaching target: current=%d target=%d: %w",
			len(p.newSlots), target, firstErr,
		)
	}

	getLogger().Info("pool: static reservation completed", ulog.F("acquired_total", len(p.newSlots)), ulog.F("in_pool", len(p.newSlots)), ulog.F("target", cap(p.newSlots)))
	return nil
}

func (p *Pool) Get(ctx context.Context, sandboxID string) (*Slot, error) {
	if sandboxID == "" {
		return nil, fmt.Errorf("sandboxID is required")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s, ok := <-p.newSlots:
		if !ok {
			return nil, fmt.Errorf("network channel has been closed")
		}
		if s != nil {
			p.signalWarmSlotNeeded()
			s.assignSandbox(sandboxID)
			if err := p.upsertSlotRecord(context.Background(), s, state.NetworkSlotAssigned, sandboxID, nil); err != nil {
				s.clearSandboxAssignment()
				if enqueueErr := p.requeueWarmSlot(s); enqueueErr != nil {
					p.markSlotCleaning(context.Background(), s, s.SandboxID(), errors.Join(err, enqueueErr))
					return nil, errors.Join(
						fmt.Errorf("failed to record assigned network slot: %w", err),
						fmt.Errorf("failed to requeue unassigned network slot: %w", enqueueErr),
					)
				}
				return nil, fmt.Errorf("failed to record assigned network slot: %w", err)
			}
			p.trackInUse(s)
		}
		return s, nil
	default:
		getLogger().Warn(
			"no available network slot in the pool",
			ulog.F("capacity", cap(p.newSlots)),
			ulog.F("in_use", p.inUseCount()),
			ulog.F("available", len(p.newSlots)),
			ulog.F("prefill_ready", p.isPrefillReady()),
			ulog.F("dynamic_reservation", p.dynamicReservation),
		)
		return nil, fmt.Errorf("no available network slot in the pool, warm_pool_size=%d", cap(p.newSlots))
	}
}

func (p *Pool) isPrefillReady() bool {
	select {
	case <-p.prefillReady:
		return true
	default:
		return false
	}
}

func (p *Pool) inUseCount() int {
	p.inUseMu.Lock()
	defer p.inUseMu.Unlock()
	return len(p.inUse)
}

func (p *Pool) requeueWarmSlot(slot *Slot) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("network channel has been closed")
		}
	}()
	select {
	case <-p.done:
		return fmt.Errorf("pool is stopping")
	case p.newSlots <- slot:
		return nil
	default:
		return fmt.Errorf("network channel is full")
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
	slot.setSlotNetwork(cniID, cniResult, opts)
	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCreating, "", nil); err != nil {
		return fmt.Errorf("failed to record cni network setup: %w", err)
	}

	if err := SetupGuestTapNetwork(ctx, slot, netnsPath, cniResult); err != nil {
		return fmt.Errorf("failed to setup guest tap network: %w", err)
	}

	return nil
}

func (p *Pool) enqueueReplacement(ctx context.Context, slot *Slot) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// Populate closes newSlots during shutdown; Release may race with that close.
			err = fmt.Errorf("network channel has been closed")
		}
	}()

	select {
	case <-p.done:
		return fmt.Errorf("pool is stopping")
	case <-ctx.Done():
		return ctx.Err()
	case p.newSlots <- slot:
		return nil
	default:
		return errWarmPoolFull
	}
}

func (p *Pool) Release(ctx context.Context, slot *Slot) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		if slot != nil {
			sandboxID := slot.SandboxID()
			slot.clearSandboxAssignment()
			slotHealth := p.slotHealth
			if p.slotHealthCheck != nil {
				slotHealth = p.slotHealthCheck
			}
			slotHealthErr := slotHealth(ctx, slot)
			if slotHealthErr == nil {
				if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotWarmIdle, "", nil); err != nil {
					slot.assignSandbox(sandboxID)
					p.trackInUse(slot)
					return fmt.Errorf("failed to record released network slot: %w", err)
				}
				p.untrackInUse(slot)
				if err := p.enqueueReplacement(ctx, slot); err != nil {
					if errors.Is(err, errWarmPoolFull) {
						if discardErr := p.discardCreatedSlot(slot); discardErr != nil {
							return fmt.Errorf("failed to discard excess released network slot: %w", discardErr)
						}
						getLogger().Info("discarded released slot because warm pool is full", ulog.F("slot_index", slot.Idx))
						return nil
					}
					slot.assignSandbox(sandboxID)
					p.trackInUse(slot)
					_ = p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotAssigned, sandboxID, err)
					return fmt.Errorf("failed to enqueue replenished slot: %w", err)
				}
				getLogger().Info("slot released back to pool", ulog.F("slot_index", slot.Idx))
			} else {
				getLogger().Warn("slot unhealthy, dropping from the pool", ulog.F("slot", slot.Key), ulog.F("error", slotHealthErr))
				if err := p.Discard(ctx, slot); err != nil {
					return fmt.Errorf("failed to discard unhealthy network slot %s: %w", slot.Key, err)
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

	var errs []error
	sandboxID := slot.SandboxID()
	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCleaning, sandboxID, nil); err != nil {
		return fmt.Errorf("failed to record cleaning network slot %s: %w", slot.Key, err)
	}
	if err := p.teardownSlotNetwork(ctx, slot); err != nil {
		getLogger().Error("failed to discard network slot", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
		getLogger().Warn("network slot still needs cleanup after failed discard", ulog.F("slot_index", slot.Idx))
		_ = p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCleaning, sandboxID, err)
		return fmt.Errorf("failed to discard network slot %s: %w", slot.Key, err)
	}

	if err := p.slotStorage.Release(slot); err != nil {
		getLogger().Error("failed to release discarded network slot", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
		_ = p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCleaning, sandboxID, err)
		errs = append(errs, fmt.Errorf("failed to release discarded network slot %s: %w", slot.Key, err))
	}
	if len(errs) == 0 {
		p.untrackInUse(slot)
		if err := p.deleteSlotRecord(context.Background(), slot); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete network slot record %s: %w", slot.Key, err))
		}
	}

	if len(errs) == 0 {
		select {
		case <-p.done:
		default:
			if err := p.replenishDroppedSlot(ctx); err != nil {
				getLogger().Warn("failed to replenish discarded network slot", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

func (p *Pool) replenishDroppedSlot(ctx context.Context) (err error) {
	var slot *Slot
	defer func() {
		if r := recover(); r != nil {
			p.discardCreatedSlot(slot)
			err = fmt.Errorf("network channel has been closed")
		}
	}()

	slot, err = p.createNetworkSlot(ctx)
	if err != nil {
		return fmt.Errorf("failed to create replacement network slot: %w", err)
	}
	select {
	case <-ctx.Done():
		p.discardCreatedSlot(slot)
		return ctx.Err()
	case <-p.done:
		p.discardCreatedSlot(slot)
		return nil
	case p.newSlots <- slot:
		return nil
	default:
		p.discardCreatedSlot(slot)
		return nil
	}
}

func (p *Pool) teardownSlotNetwork(ctx context.Context, slot *Slot) error {
	if slot == nil {
		return nil
	}

	var errs []error
	netnsPath := slot.NetNSPath()
	var cniErr error
	cniID := slot.CNIContainerID()
	cniIP := ""
	cniTeardownComplete := slot.CNIResult() == nil
	if slot.CNIResult() != nil {
		if slot.CNIResult().IP != "" {
			cniIP = slot.CNIResult().IP
		}
		if err := TeardownGuestTapNetwork(ctx, slot, netnsPath, slot.CNIResult()); err != nil {
			errs = append(errs, err)
		}
		if p != nil && p.cniManager != nil {
			cniErr = p.teardownSandboxNetworkWithRetry(ctx, slot, netnsPath)
		}
		if cniErr == nil {
			slot.clearSlotNetwork()
			cniTeardownComplete = true
		}
	}
	slot.clearSandboxAssignment()

	if cniErr != nil {
		if isCNIBusyTeardownError(cniErr) {
			if p.validateBusyCNITeardownClean(cniErr, slot, cniID, cniIP) == nil {
				getLogger().Warn("cni teardown reported busy state after cleanup completed", ulog.F("slot_index", slot.Idx), ulog.F("error", cniErr))
				cniTeardownComplete = true
			} else {
				errs = append(errs, cniErr)
			}
		} else {
			errs = append(errs, cniErr)
		}
	}
	if cniTeardownComplete {
		if err := DeleteSandboxNetworkNamespace(netnsPath); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (p *Pool) teardownSandboxNetworkWithRetry(ctx context.Context, slot *Slot, netnsPath string) error {
	var lastErr error
	for attempt := 0; attempt <= cniTeardownRetryAttempts; attempt++ {
		err := p.cniManager.TeardownSandboxNetwork(ctx, slot.CNIContainerID(), netnsPath, slot.cniOpts...)
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

func (p *Pool) validateBusyCNITeardownClean(cniErr error, slot *Slot, cniID, cniIP string) error {
	if chain := cniBusyChain(cniErr); chain != "" {
		if err := validateIPTablesChainDeleted("nat", chain); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("cannot validate busy cni teardown without chain name")
	}
	if cniID == "" {
		return fmt.Errorf("missing cni id")
	}
	if p == nil || p.cniManager == nil {
		return fmt.Errorf("missing cni manager")
	}

	allocDir := p.cniManager.selectedHostLocalAllocDir
	if allocDir == "" {
		return fmt.Errorf("selected cni config does not use host-local ipam")
	}
	entries, err := os.ReadDir(allocDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading host-local allocation dir %s: %w", allocDir, err)
	}

	if cniIP != "" {
		ipFile := cniIP
		if ip, _, ok := strings.Cut(cniIP, "/"); ok {
			ipFile = ip
		}
		if _, err := os.Stat(filepath.Join(allocDir, ipFile)); err == nil {
			return fmt.Errorf("host-local allocation %s still exists for cni id %s", ipFile, cniID)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking host-local allocation %s: %w", ipFile, err)
		}
	}

	for _, entry := range entries {
		// host-local stores cursor/lock metadata next to IP allocation files; only allocation files can belong to a CNI ID.
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "last_reserved_ip") || entry.Name() == "lock" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(allocDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("reading host-local allocation %s: %w", entry.Name(), err)
		}
		if strings.Contains(string(content), cniID) {
			return fmt.Errorf("host-local allocation %s still references cni id %s", entry.Name(), cniID)
		}
	}

	return nil
}

func (p *Pool) validateHostLocalAllocationOwned(cniID, cniIP string) error {
	if p == nil || p.cniManager == nil || p.cniManager.selectedHostLocalAllocDir == "" {
		return nil
	}
	if cniID == "" || cniIP == "" {
		return fmt.Errorf("missing cni id or ip")
	}
	ipFile := cniIP
	if ip, _, ok := strings.Cut(cniIP, "/"); ok {
		ipFile = ip
	}
	path := filepath.Join(p.cniManager.selectedHostLocalAllocDir, ipFile)
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading host-local allocation %s: %w", path, err)
	}
	if !strings.Contains(string(content), cniID) {
		return fmt.Errorf("host-local allocation %s does not reference cni id %s", path, cniID)
	}
	return nil
}

func cniBusyChain(err error) string {
	if err == nil {
		return ""
	}
	fields := strings.Fields(err.Error())
	for i, field := range fields {
		cleaned := strings.Trim(field, `"'.,;:()[]`)
		if strings.HasPrefix(cleaned, "CNI-") {
			return cleaned
		}
		if strings.EqualFold(cleaned, "chain") && i+1 < len(fields) {
			next := strings.Trim(fields[i+1], `"'.,;:()[]`)
			if strings.HasPrefix(next, "CNI-") {
				return next
			}
		}
	}
	return ""
}

func validateIPTablesChainDeleted(table, chain string) error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}
	chains, err := tables.ListChains(table)
	if err != nil {
		return fmt.Errorf("checking iptables %s chains: %w", table, err)
	}
	for _, existing := range chains {
		if existing == chain {
			return fmt.Errorf("iptables %s chain %s still exists after cni teardown", table, chain)
		}
	}
	return nil
}

func (p *Pool) trackInUse(slot *Slot) {
	p.inUseMu.Lock()
	defer p.inUseMu.Unlock()
	p.inUse[slot.Key] = slot
}

func (p *Pool) RestoreInUse(slot *Slot, sandboxID, ip string) error {
	if p == nil {
		return fmt.Errorf("pool is nil")
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	if strings.TrimSpace(sandboxID) == "" {
		return fmt.Errorf("sandbox id is required")
	}
	if strings.TrimSpace(ip) == "" {
		return fmt.Errorf("cni ip is required")
	}
	if p.slotStorage != nil {
		if err := p.slotStorage.Claim(slot); err != nil {
			return fmt.Errorf("claim restored network slot %s: %w", slot.Key, err)
		}
	}
	cniID := slot.CNIContainerID()
	slot.setSlotNetwork(cniID, &CNIResult{IP: ip}, nil)
	fail := func(err error) error {
		if writeErr := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCleaning, sandboxID, err); writeErr != nil {
			getLogger().Warn("failed to mark rehydrated network slot cleaning", ulog.F("slot_index", slot.Idx), ulog.F("error", writeErr))
		}
		slot.clearSlotNetwork()
		return err
	}
	if p.cniManager == nil {
		return fail(fmt.Errorf("cni config not initialized"))
	}
	if _, err := os.Stat(slot.NetNSPath()); err != nil {
		return fail(fmt.Errorf("namespace missing: %w", err))
	}
	opts, err := buildCNIOpts(slot, cniID, slot.NetNSPath())
	if err != nil {
		return fail(err)
	}
	slot.setSlotNetwork(cniID, &CNIResult{IP: ip}, opts)
	if err := ValidateReusableSlotNetwork(context.Background(), slot, slot.NetNSPath(), p.cniManager.config.IfName); err != nil {
		return fail(fmt.Errorf("rehydrated network slot is not reusable: %w", err))
	}
	if err := p.validateHostLocalAllocationOwned(cniID, ip); err != nil {
		return fail(fmt.Errorf("rehydrated network slot has invalid ipam state: %w", err))
	}
	slot.assignSandbox(sandboxID)
	if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotAssigned, sandboxID, nil); err != nil {
		slot.clearSandboxAssignment()
		return fmt.Errorf("failed to record rehydrated network slot: %w", err)
	}
	p.trackInUse(slot)
	return nil
}

func (p *Pool) AdoptWarmIdle(ctx context.Context) (int, error) {
	if p == nil || p.slotStore == nil {
		return 0, nil
	}
	records, err := p.slotStore.ListNetworkSlots(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrNetworkSlotStoreRead, err)
	}
	adopted := 0
	var errs []error
	// Reserve assigned slots before warm slots so persisted assignments never
	// displace a network that may still belong to a running sandbox.
	for _, rec := range records {
		if rec.State != state.NetworkSlotAssigned {
			continue
		}
		if err := p.slotStorage.Claim(recordSlotFromState(rec)); err != nil {
			return 0, fmt.Errorf("%w: claim assigned slot %s: %v", ErrNetworkSlotCapacity, rec.SlotKey, err)
		}
	}
	for _, rec := range records {
		switch rec.State {
		case state.NetworkSlotCreating:
			cleaningErr := fmt.Errorf("slot was interrupted while creating")
			if err := p.upsertSlotRecord(ctx, recordSlotFromState(rec), state.NetworkSlotCleaning, rec.SandboxID, cleaningErr); err != nil {
				errs = append(errs, fmt.Errorf("mark creating slot %s cleaning: %w", rec.SlotKey, err))
				continue
			}
			if err := p.cleanupRecordedSlot(ctx, rec, "startup creating"); err != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup creating slot %s: %v", ErrNetworkSlotCleanup, rec.SlotKey, err))
			}
			continue
		case state.NetworkSlotCleaning:
			if err := p.cleanupRecordedSlot(ctx, rec, "startup cleaning"); err != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup recorded slot %s: %v", ErrNetworkSlotCleanup, rec.SlotKey, err))
			}
			continue
		case state.NetworkSlotWarmIdle:
		case state.NetworkSlotAssigned:
			continue
		default:
			continue
		}
		slot, err := slotFromNetworkSlotRecord(rec)
		if err != nil {
			errs = append(errs, fmt.Errorf("restore warm slot %s: %w", rec.SlotKey, err))
			continue
		}
		if err := p.slotStorage.Claim(slot); err != nil {
			if cleanupErr := p.cleanupRecordedSlot(ctx, rec, "startup warm slot exceeds capacity"); cleanupErr != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup overflow warm slot %s: %v", ErrNetworkSlotCleanup, rec.SlotKey, cleanupErr))
			}
			continue
		}
		if err := ValidateReusableSlotNetwork(ctx, slot, slot.NetNSPath(), p.cniManager.config.IfName); err != nil {
			if cleanupErr := p.cleanupRecordedSlot(ctx, rec, "startup warm validation failed"); cleanupErr != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup invalid warm slot %s: %v", ErrNetworkSlotCleanup, rec.SlotKey, cleanupErr))
			}
			continue
		}
		if err := p.validateHostLocalAllocationOwned(slot.CNIContainerID(), rec.CNIIP); err != nil {
			if cleanupErr := p.cleanupRecordedSlot(ctx, rec, "startup warm ipam validation failed"); cleanupErr != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup invalid warm slot ipam %s: %v", ErrNetworkSlotCleanup, rec.SlotKey, cleanupErr))
			}
			continue
		}
		select {
		case <-ctx.Done():
			return adopted, ctx.Err()
		case p.newSlots <- slot:
			adopted++
		default:
			if cleanupErr := p.cleanupRecordedSlot(ctx, rec, "startup warm queue full"); cleanupErr != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup overflow warm slot %s: %v", ErrNetworkSlotCleanup, rec.SlotKey, cleanupErr))
			}
		}
	}
	if adopted > 0 {
		getLogger().Info("adopted warm network slots", ulog.F("count", adopted))
	}
	return adopted, errors.Join(errs...)
}

func (p *Pool) CleanupAssignedWithoutReadySandbox(readySandboxIDs map[string]struct{}) error {
	if p == nil || p.slotStore == nil {
		return nil
	}
	records, err := p.slotStore.ListNetworkSlots(context.Background())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNetworkSlotStoreRead, err)
	}
	var errs []error
	for _, rec := range records {
		if rec.State != state.NetworkSlotAssigned {
			continue
		}
		if _, ok := readySandboxIDs[rec.SandboxID]; ok {
			continue
		}
		if err := p.cleanupRecordedSlot(context.Background(), rec, "startup assigned without ready sandbox"); err != nil {
			errs = append(errs, fmt.Errorf("%w: cleanup assigned slot %s: %v", ErrNetworkSlotCleanup, rec.SlotKey, err))
		}
	}
	return errors.Join(errs...)
}

func (p *Pool) cleanupRecordedSlot(ctx context.Context, rec state.NetworkSlotRecord, reason string) error {
	slot, err := slotFromNetworkSlotRecord(rec)
	if err != nil {
		return err
	}
	sandboxID := rec.SandboxID
	if err := p.upsertSlotRecord(ctx, slot, state.NetworkSlotCleaning, sandboxID, errors.New(reason)); err != nil {
		return fmt.Errorf("record cleaning state: %w", err)
	}
	if err := p.teardownSlotNetwork(ctx, slot); err != nil {
		p.markSlotCleaning(context.Background(), slot, sandboxID, err)
		return fmt.Errorf("teardown network: %w", err)
	}
	if err := p.slotStorage.Release(slot); err != nil {
		p.markSlotCleaning(context.Background(), slot, sandboxID, err)
		return fmt.Errorf("release storage: %w", err)
	}
	if err := p.deleteSlotRecord(context.Background(), slot); err != nil {
		return fmt.Errorf("delete slot record: %w", err)
	}
	return nil
}

func slotFromNetworkSlotRecord(rec state.NetworkSlotRecord) (*Slot, error) {
	slot, err := NewSlot(rec.SlotKey, rec.SlotIndex)
	if err != nil {
		return nil, err
	}
	if rec.NetNSPath != "" {
		slot.setNetNSPath(rec.NetNSPath)
	}
	cniID := rec.CNIID
	if cniID == "" {
		cniID = slot.CNIContainerID()
	}
	opts, err := buildCNIOpts(slot, cniID, slot.NetNSPath())
	if err != nil {
		return nil, err
	}
	if rec.CNIIP != "" {
		slot.setSlotNetwork(cniID, &CNIResult{IP: rec.CNIIP}, opts)
	}
	return slot, nil
}

func recordSlotFromState(rec state.NetworkSlotRecord) *Slot {
	slot := &Slot{Key: rec.SlotKey, Idx: rec.SlotIndex}
	if rec.NetNSPath != "" {
		slot.setNetNSPath(rec.NetNSPath)
	}
	if rec.CNIIP != "" {
		cniID := rec.CNIID
		if cniID == "" {
			cniID = slot.CNIContainerID()
		}
		slot.setSlotNetwork(cniID, &CNIResult{IP: rec.CNIIP}, nil)
	}
	if rec.SandboxID != "" {
		slot.assignSandbox(rec.SandboxID)
	}
	return slot
}

func (p *Pool) untrackInUse(slot *Slot) {
	p.inUseMu.Lock()
	defer p.inUseMu.Unlock()
	delete(p.inUse, slot.Key)
}

func (p *Pool) drainInUse() []*Slot {
	p.inUseMu.Lock()
	defer p.inUseMu.Unlock()
	slots := make([]*Slot, 0, len(p.inUse))
	for key, slot := range p.inUse {
		slots = append(slots, slot)
		delete(p.inUse, key)
	}
	return slots
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
	if err := p.validateHostLocalAllocationOwned(slot.CNIContainerID(), slot.CNIResult().IP); err != nil {
		return err
	}
	return nil
}

func (p *Pool) stopPopulate() {
	p.stopOnce.Do(func() {
		if p.done == nil {
			return
		}
		p.populateMu.Lock()
		cancel := p.populateCancel
		p.populateMu.Unlock()
		if cancel != nil {
			cancel(errPoolCleanupRequested)
		}
		close(p.done)
	})
}

func (p *Pool) WaitPopulateStopped() {
	p.waitPopulateStopped(true)
}

func (p *Pool) waitPopulateStopped(requireCanceled bool) {
	p.populateMu.Lock()
	started := p.populateStarted
	ctx := p.populateCtx
	done := p.populateDone
	p.populateMu.Unlock()

	if !started || done == nil {
		return
	}
	if requireCanceled && (ctx == nil || ctx.Err() == nil) {
		return
	}
	<-done
}

func (p *Pool) Cleanup() error {
	p.stopPopulate()
	p.waitPopulateStopped(false)

	var errs []error
	cleaned := 0
	failed := 0
	cleanupSlot := func(slot *Slot, category string) {
		if slot == nil {
			failed++
			return
		}
		sandboxID := slot.SandboxID()
		if err := p.upsertSlotRecord(context.Background(), slot, state.NetworkSlotCleaning, sandboxID, nil); err != nil {
			getLogger().Error("cleanup slot failed when recording cleaning state", ulog.F("slot", slot.Key), ulog.F("category", category), ulog.F("error", err))
			errs = append(errs, fmt.Errorf("cleanup slot %s record cleaning failed, %w", slot.Key, err))
			failed++
			return
		}
		err := p.teardownSlotNetwork(context.Background(), slot)
		if err != nil {
			getLogger().Error("cleanup slot failed when removing network", ulog.F("slot", slot.Key), ulog.F("category", category), ulog.F("error", err))
			errs = append(errs, fmt.Errorf("cleanup slot %s failed, %w", slot.Key, err))
			p.markSlotCleaning(context.Background(), slot, sandboxID, err)
			p.trackInUse(slot)
			failed++
			return
		}
		err = p.slotStorage.Release(slot)
		if err != nil {
			getLogger().Error("cleanup slot failed when releasing", ulog.F("slot", slot.Key), ulog.F("category", category), ulog.F("error", err))
			errs = append(errs, fmt.Errorf("cleanup slot %s failed, %w", slot.Key, err))
			p.markSlotCleaning(context.Background(), slot, sandboxID, err)
			p.trackInUse(slot)
			failed++
			return
		}
		if err := p.deleteSlotRecord(context.Background(), slot); err != nil {
			getLogger().Error("cleanup slot failed when deleting record", ulog.F("slot", slot.Key), ulog.F("category", category), ulog.F("error", err))
			errs = append(errs, fmt.Errorf("cleanup slot %s record failed, %w", slot.Key, err))
			failed++
			return
		}
		cleaned++
	}

	finishInUse := cleanupdiag.Start("network_pool.cleanup_in_use")
	inUseSlots := p.drainInUse()
	for _, slot := range inUseSlots {
		cleanupSlot(slot, "in_use")
	}
	finishInUse(nil)

	finishQueue := cleanupdiag.Start("network_pool.cleanup_queue", ulog.F("queued_slots", len(p.newSlots)), ulog.F("queue_capacity", cap(p.newSlots)))
	queueCleaned := 0
	draining := true
	for draining {
		select {
		case slot, ok := <-p.newSlots:
			if !ok {
				draining = false
				continue
			}
			cleanupSlot(slot, "queue")
			queueCleaned++
		default:
			draining = false
		}
	}
	finishQueue(nil)

	finishRecords := cleanupdiag.Start("network_pool.cleanup_recorded")
	if p.slotStore != nil {
		records, err := p.slotStore.ListNetworkSlots(context.Background())
		if err != nil {
			errs = append(errs, fmt.Errorf("list recorded network slots: %w", err))
			failed++
		} else {
			for _, rec := range records {
				slot, err := slotFromNetworkSlotRecord(rec)
				if err != nil {
					errs = append(errs, fmt.Errorf("restore recorded network slot %s: %w", rec.SlotKey, err))
					failed++
					continue
				}
				cleanupSlot(slot, "record")
			}
		}
	}
	finishRecords(nil)

	if failed == 0 {
		if err := p.cleanupCNIHostArtifacts(context.Background()); err != nil {
			getLogger().Error("cleanup cni host artifacts failed", ulog.F("error", err))
			errs = append(errs, err)
		}
	} else {
		getLogger().Warn("skipping cni host artifact cleanup because slot cleanup failed", ulog.F("failed_slots", failed))
	}

	getLogger().Info("pool cleanup summary",
		ulog.F("cleaned_slots", cleaned),
		ulog.F("failed_slots", failed),
		ulog.F("in_use_slots", len(inUseSlots)),
		ulog.F("queue_slots", queueCleaned),
	)
	return errors.Join(errs...)
}

func (p *Pool) cleanupCNIHostArtifacts(ctx context.Context) error {
	if p == nil || p.cniManager == nil {
		return nil
	}
	bridges, err := p.cniManager.SelectedBridgeNames()
	if err != nil {
		return fmt.Errorf("finding cni bridge artifacts: %w", err)
	}
	var errs []error
	for _, bridgeName := range bridges {
		if err := deleteEmptyCNIHostBridge(ctx, bridgeName, cniBridgeCleanupRetries, cniBridgeCleanupDelay); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
