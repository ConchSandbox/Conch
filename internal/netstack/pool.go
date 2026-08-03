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
	"github.com/openeuler/Conch/internal/daemon/state"
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
	ErrNetworkSlotStoreRead = errors.New("network slot store read failed")
	ErrNetworkSlotCleanup   = errors.New("network slot cleanup failed")
	ErrNetworkSlotCapacity  = slotstate.ErrCapacity
	errWarmPoolClosed       = slotstate.ErrClosed
	errWarmPoolEmpty        = slotstate.ErrEmpty
)

func getLogger() ulog.Logger {
	return ulog.GetLogger()
}

type NetworkSlotStore interface {
	CreateNetworkSlot(context.Context, state.NetworkSlotRecord) error
	UpdateNetworkSlot(context.Context, state.NetworkSlotRecord) error
	GetNetworkSlot(context.Context, int) (state.NetworkSlotRecord, error)
	ListNetworkSlots(context.Context) ([]state.NetworkSlotRecord, error)
	DeleteNetworkSlot(context.Context, int) error
}

type preserveOnCancelContextKey struct{}

func withPreserveOnCancel(ctx context.Context) context.Context {
	return context.WithValue(ctx, preserveOnCancelContextKey{}, true)
}

type Pool struct {
	warmSlots          *slotstate.Queue[*Slot]
	warmSlotNeeded     chan struct{}
	populateCancel     context.CancelFunc
	populateDone       <-chan struct{}
	dynamicReservation bool
	cniManager         *CNIManager
	slotStore          NetworkSlotStore
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

func NewPool(warmPoolSize int, dynamicReservation bool, tapIP string, tapMask int, cniCfg CNIManagerConfig, slotStore NetworkSlotStore) (*Pool, error) {
	if slotStore == nil {
		return nil, fmt.Errorf("network slot store is required")
	}
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
		slotStore:          slotStore,
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
		if isExpectedShutdownError(ctx, err) {
			return nil, context.Canceled
		}
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
	rec := state.NetworkSlotRecord{
		SlotID:    id,
		State:     state.NetworkSlotTransient,
		UpdatedAt: time.Now().UnixNano(),
	}
	if err := p.slotStore.CreateNetworkSlot(ctx, rec); err != nil {
		if errors.Is(err, state.ErrAlreadyExists) {
			return nil, fmt.Errorf("persist network slot %d: allocator and store are out of sync: %w", id, err)
		}
		return nil, errors.Join(fmt.Errorf("persist network slot %d: %w", id, err), p.slotIDs.Release(id))
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, p.deleteSlotRecord(context.Background(), slot))
	}

	handleCreationFailure := func(cause error) error {
		if isExpectedShutdownError(ctx, cause) {
			p.markSlotTransient(context.Background(), slot, slot.SandboxID(), cause)
			getLogger().Debug(
				"network slot creation interrupted during shutdown",
				ulog.F("slot_id", slot.ID),
				ulog.F("error", cause),
			)
			return errors.Join(context.Canceled, cause)
		}

		if cleanupErr := p.cleanupSlotAllocation(slot, cause); cleanupErr != nil {
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
	if err := p.updateSlotRecord(context.Background(), slot, state.NetworkSlotIdle, "", nil); err != nil {
		return fmt.Errorf("record warm network slot %d: %w", slot.ID, err)
	}
	return nil
}

func (p *Pool) cleanupSlotAllocation(slot *Slot, cause error) error {
	if slot == nil {
		return nil
	}
	var errs []error
	if err := p.updateSlotRecord(context.Background(), slot, state.NetworkSlotTransient, slot.SandboxID(), cause); err != nil {
		errs = append(errs, fmt.Errorf("record network slot cleanup: %w", err))
	}
	if err := p.teardownSlotNetwork(context.Background(), slot); err != nil {
		p.markSlotTransient(context.Background(), slot, slot.SandboxID(), errors.Join(cause, err))
		return errors.Join(append(errs, fmt.Errorf("teardown network slot: %w", err))...)
	}
	if err := p.deleteSlotRecord(context.Background(), slot); err != nil {
		p.markSlotTransient(context.Background(), slot, slot.SandboxID(), errors.Join(cause, err))
		errs = append(errs, fmt.Errorf("delete network slot record: %w", err))
	}
	return errors.Join(errs...)
}

func shouldPreserveAfterCancel(ctx context.Context) bool {
	if ctx == nil || ctx.Err() == nil {
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

func (p *Pool) updateSlotRecord(ctx context.Context, slot *Slot, slotState, sandboxID string, err error) error {
	if p == nil || p.slotStore == nil || slot == nil {
		return nil
	}
	rec := state.NetworkSlotRecord{
		SlotID:    slot.ID,
		State:     slotState,
		SandboxID: sandboxID,
		UpdatedAt: time.Now().UnixNano(),
	}
	if result := slot.CNIResult(); result != nil {
		rec.CNIIP = result.IP
	}
	if err != nil {
		rec.LastError = err.Error()
	}
	return p.slotStore.UpdateNetworkSlot(ctx, rec)
}

func (p *Pool) deleteSlotRecord(ctx context.Context, slot *Slot) error {
	if p == nil || p.slotStore == nil || slot == nil {
		return nil
	}
	if err := p.slotStore.DeleteNetworkSlot(ctx, slot.ID); err != nil {
		return err
	}
	if p.slotIDs == nil {
		return fmt.Errorf("network slot ID allocator is not initialized")
	}
	if err := p.slotIDs.Release(slot.ID); err != nil {
		return fmt.Errorf("release network slot ID %d: %w", slot.ID, err)
	}
	return nil
}

func (p *Pool) markSlotTransient(ctx context.Context, slot *Slot, sandboxID string, err error) {
	if writeErr := p.updateSlotRecord(ctx, slot, state.NetworkSlotTransient, sandboxID, err); writeErr != nil {
		getLogger().Warn("failed to mark network slot transient", ulog.F("slot_id", slot.ID), ulog.F("error", writeErr))
	}
}

// Start launches the pool's population loop. It must be called exactly once.
func (p *Pool) Start(ctx context.Context) {
	populateCtx, populateCancel := context.WithCancel(withPreserveOnCancel(ctx))
	populateDone := make(chan struct{})
	p.populateCancel = populateCancel
	p.populateDone = populateDone
	go func() {
		defer close(populateDone)
		p.populate(populateCtx)
	}()
}

// Close prevents further queue access, stops the population loop, and waits
// for it to exit. Slots already buffered remain persisted for recovery by the
// next daemon instance.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	if p.warmSlots != nil {
		p.warmSlots.Close()
	}
	if p.populateCancel == nil || p.populateDone == nil {
		return
	}
	p.populateCancel()
	<-p.populateDone
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
	if err := p.cleanupSlotAllocation(slot, nil); err != nil {
		getLogger().Warn("failed to discard unqueued network slot", ulog.F("slot_id", slot.ID), ulog.F("error", err))
		return err
	}
	return nil
}

func (p *Pool) preserveCreatedSlot(slot *Slot) {
	if slot == nil {
		return
	}
	if err := p.updateSlotRecord(context.Background(), slot, state.NetworkSlotIdle, "", nil); err != nil {
		getLogger().Warn("failed to preserve network slot record during shutdown", ulog.F("slot_id", slot.ID), ulog.F("error", err))
	}
}

func (p *Pool) handleCreatedSlotAfterCancel(ctx context.Context, slot *Slot) {
	if shouldPreserveAfterCancel(ctx) {
		p.preserveCreatedSlot(slot)
		return
	}
	_ = p.discardSlotWithoutReplacement(slot)
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
						p.handleCreatedSlotAfterCancel(ctx, slot)
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
					p.handleCreatedSlotAfterCancel(ctx, r.slot)
				}
			}
			return ctx.Err()
		case jobs <- job{}:
		}
	}
	close(jobs)

	var firstErr error
	preserving := false
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
		if preserving {
			p.handleCreatedSlotAfterCancel(ctx, r.slot)
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
			preserving = true
		}
	}
	if queueClosed {
		return errWarmPoolClosed
	}
	if preserving {
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
	if err := p.updateSlotRecord(context.Background(), s, state.NetworkSlotAssigned, sandboxID, nil); err != nil {
		s.clearSandboxAssignment()
		if enqueueErr := p.warmSlots.Push(s); enqueueErr != nil {
			discardErr := p.discardSlotWithoutReplacement(s)
			joinedErr := errors.Join(
				fmt.Errorf("failed to record assigned network slot: %w", err),
				fmt.Errorf("failed to requeue unassigned network slot: %w", enqueueErr),
			)
			if discardErr != nil {
				joinedErr = errors.Join(joinedErr, fmt.Errorf("failed to discard unqueued network slot: %w", discardErr))
			}
			return nil, joinedErr
		}
		return nil, fmt.Errorf("failed to record assigned network slot: %w", err)
	}
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

	// createNetworkSlot persists TRANSIENT before any OS resource is created.
	// Because the CNI ID, namespace path, and options are derived from Slot ID,
	// that one record is sufficient for recovery to issue an idempotent DEL.
	cniResult, err := p.cniManager.SetupSandboxNetwork(ctx, cniID, netnsPath, opts...)
	if err != nil {
		return fmt.Errorf("failed to setup cni network: %w", err)
	}
	slot.setCNIResult(cniResult)
	if err := p.updateSlotRecord(context.Background(), slot, state.NetworkSlotTransient, "", nil); err != nil {
		return fmt.Errorf("failed to record cni network setup: %w", err)
	}

	if err := SetupGuestTapNetwork(ctx, slot, netnsPath, cniResult); err != nil {
		return fmt.Errorf("failed to setup guest tap network: %w", err)
	}

	return nil
}

func (p *Pool) Release(ctx context.Context, slot *Slot) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		if slot != nil {
			sandboxID := slot.SandboxID()
			slot.clearSandboxAssignment()
			slotHealthErr := p.slotHealth(ctx, slot)
			if slotHealthErr == nil {
				if err := p.updateSlotRecord(context.Background(), slot, state.NetworkSlotIdle, "", nil); err != nil {
					slot.assignSandbox(sandboxID)
					return fmt.Errorf("failed to record released network slot: %w", err)
				}
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

	sandboxID := slot.SandboxID()
	if err := p.updateSlotRecord(context.Background(), slot, state.NetworkSlotTransient, sandboxID, nil); err != nil {
		return fmt.Errorf("failed to record transient network slot %d: %w", slot.ID, err)
	}
	if err := p.teardownSlotNetwork(ctx, slot); err != nil {
		getLogger().Error("failed to discard network slot", ulog.F("slot_id", slot.ID), ulog.F("error", err))
		getLogger().Warn("network slot still needs cleanup after failed discard", ulog.F("slot_id", slot.ID))
		_ = p.updateSlotRecord(context.Background(), slot, state.NetworkSlotTransient, sandboxID, err)
		return fmt.Errorf("failed to discard network slot %d: %w", slot.ID, err)
	}

	if err := p.deleteSlotRecord(context.Background(), slot); err != nil {
		p.markSlotTransient(context.Background(), slot, sandboxID, err)
		return fmt.Errorf("failed to delete network slot record %d: %w", slot.ID, err)
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
	cniID := slot.CNIContainerID()
	cniIP := ""
	if result := slot.CNIResult(); result != nil {
		cniIP = result.IP
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
	}
	if cniErr != nil {
		if isCNIBusyTeardownError(cniErr) {
			if p.validateBusyCNITeardownClean(cniErr, slot, cniID, cniIP) == nil {
				getLogger().Warn("cni teardown reported busy state after cleanup completed", ulog.F("slot_id", slot.ID), ulog.F("error", cniErr))
				cniTeardownComplete = true
			} else {
				errs = append(errs, cniErr)
			}
		} else {
			errs = append(errs, cniErr)
		}
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

func (p *Pool) RestoreAssigned(slot *Slot, sandboxID, ip string) error {
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
	if p.slotStore == nil {
		return fmt.Errorf("network slot store is required")
	}
	rec, err := p.slotStore.GetNetworkSlot(context.Background(), slot.ID)
	if err != nil {
		return fmt.Errorf("read restored network slot %d: %w", slot.ID, err)
	}
	if rec.State != state.NetworkSlotAssigned {
		return fmt.Errorf("restored network slot %d state is %s, want %s", slot.ID, rec.State, state.NetworkSlotAssigned)
	}
	if rec.SandboxID != sandboxID {
		return fmt.Errorf("restored network slot %d belongs to sandbox %s, not %s", slot.ID, rec.SandboxID, sandboxID)
	}
	if rec.CNIIP == "" || rec.CNIIP != ip {
		return fmt.Errorf("restored network slot %d IP mismatch: record=%q sandbox=%q", slot.ID, rec.CNIIP, ip)
	}
	cniID := slot.CNIContainerID()
	slot.setCNIResult(&CNIResult{IP: ip})
	fail := func(err error) error {
		slot.clearCNIResult()
		return err
	}
	if p.cniManager == nil {
		return fail(fmt.Errorf("cni config not initialized"))
	}
	if _, err := os.Stat(slot.NetNSPath()); err != nil {
		return fail(fmt.Errorf("namespace missing: %w", err))
	}
	if err := ValidateReusableSlotNetwork(context.Background(), slot, slot.NetNSPath(), p.cniManager.config.IfName); err != nil {
		return fail(fmt.Errorf("rehydrated network slot is not reusable: %w", err))
	}
	if err := p.validateHostLocalAllocationOwned(cniID, ip); err != nil {
		return fail(fmt.Errorf("rehydrated network slot has invalid ipam state: %w", err))
	}
	slot.assignSandbox(sandboxID)
	if err := p.updateSlotRecord(context.Background(), slot, state.NetworkSlotAssigned, sandboxID, nil); err != nil {
		slot.clearSandboxAssignment()
		return fmt.Errorf("failed to record rehydrated network slot: %w", err)
	}
	return nil
}

// AdoptIdle rebuilds Slot ID allocation state and recovers persisted
// slots. It must complete before Start launches any Slot creation workers.
func (p *Pool) AdoptIdle(ctx context.Context) (int, error) {
	if p == nil || p.slotStore == nil {
		return 0, nil
	}
	records, err := p.slotStore.ListNetworkSlots(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrNetworkSlotStoreRead, err)
	}
	if p.slotIDs == nil {
		p.slotIDs = slotstate.NewAllocator(firstSlotID, maxSlots)
	}
	usedIDs := make([]int, 0, len(records))
	for _, rec := range records {
		usedIDs = append(usedIDs, rec.SlotID)
	}
	if err := p.slotIDs.Rebuild(usedIDs); err != nil {
		return 0, fmt.Errorf("%w: rebuild slot ID allocator: %v", ErrNetworkSlotStoreRead, err)
	}
	adopted := 0
	var errs []error
	for _, rec := range records {
		switch rec.State {
		case state.NetworkSlotTransient:
			if err := p.cleanupRecordedSlot(ctx, rec, "startup transient"); err != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup transient slot %d: %v", ErrNetworkSlotCleanup, rec.SlotID, err))
			}
			continue
		case state.NetworkSlotIdle:
		case state.NetworkSlotAssigned:
			continue
		default:
			continue
		}
		slot, err := slotFromNetworkSlotRecord(rec)
		if err != nil {
			errs = append(errs, fmt.Errorf("restore warm slot %d: %w", rec.SlotID, err))
			continue
		}
		if err := ValidateReusableSlotNetwork(ctx, slot, slot.NetNSPath(), p.cniManager.config.IfName); err != nil {
			if cleanupErr := p.cleanupRecordedSlot(ctx, rec, "startup warm validation failed"); cleanupErr != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup invalid warm slot %d: %v", ErrNetworkSlotCleanup, rec.SlotID, cleanupErr))
			}
			continue
		}
		if err := p.validateHostLocalAllocationOwned(slot.CNIContainerID(), rec.CNIIP); err != nil {
			if cleanupErr := p.cleanupRecordedSlot(ctx, rec, "startup warm ipam validation failed"); cleanupErr != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup invalid warm slot ipam %d: %v", ErrNetworkSlotCleanup, rec.SlotID, cleanupErr))
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return adopted, err
		}
		if enqueueErr := p.warmSlots.Push(slot); enqueueErr == nil {
			adopted++
		} else {
			reason := "startup warm queue full"
			if errors.Is(enqueueErr, errWarmPoolClosed) {
				reason = "startup warm queue closed"
				errs = append(errs, enqueueErr)
			}
			if cleanupErr := p.cleanupRecordedSlot(ctx, rec, reason); cleanupErr != nil {
				errs = append(errs, fmt.Errorf("%w: cleanup overflow warm slot %d: %v", ErrNetworkSlotCleanup, rec.SlotID, cleanupErr))
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
			errs = append(errs, fmt.Errorf("%w: cleanup assigned slot %d: %v", ErrNetworkSlotCleanup, rec.SlotID, err))
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
	if err := p.updateSlotRecord(ctx, slot, state.NetworkSlotTransient, sandboxID, errors.New(reason)); err != nil {
		return fmt.Errorf("record transient state: %w", err)
	}
	if err := p.teardownSlotNetwork(ctx, slot); err != nil {
		p.markSlotTransient(context.Background(), slot, sandboxID, err)
		return fmt.Errorf("teardown network: %w", err)
	}
	if err := p.deleteSlotRecord(context.Background(), slot); err != nil {
		p.markSlotTransient(context.Background(), slot, sandboxID, err)
		return fmt.Errorf("delete slot record: %w", err)
	}
	return nil
}

func slotFromNetworkSlotRecord(rec state.NetworkSlotRecord) (*Slot, error) {
	slot, err := NewSlot(rec.SlotID)
	if err != nil {
		return nil, err
	}
	if rec.CNIIP != "" {
		slot.setCNIResult(&CNIResult{IP: rec.CNIIP})
	}
	return slot, nil
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
