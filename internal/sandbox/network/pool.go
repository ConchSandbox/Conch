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
*/
package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"github.com/openeuler/Conch/pkg/ulog"
	"github.com/vishvananda/netlink"
)

const (
	defaultPoolSize = 250
	prefillWorkers  = 16
)

func getLogger() ulog.Logger {
	return ulog.GetLogger()
}

type Pool struct {
	slotStorage        Storage
	newSlots           chan *Slot
	done               chan struct{}
	prefillReady       chan struct{}
	prefillReadyOnce   sync.Once
	dynamicReservation bool
	inUse              map[string]*Slot
	inUseMu            sync.Mutex
}

func initHostMasquerade() error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}
	if err := tables.AppendUnique("nat", "POSTROUTING", "-s", vrtNetworkCIDR.String(), "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("error creating postrouting rule: %w", err)
	}
	getLogger().Debug("host masquerade initialized", ulog.F("cidr", vrtNetworkCIDR.String()))
	return nil
}

func deleteHostMasquerade() error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}
	if err := tables.Delete("nat", "POSTROUTING", "-s", vrtNetworkCIDR.String(), "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("error deleting postrouting rule: %w", err)
	}
	return nil
}

func normalizeAndValidatePoolSize(poolSize int) (int, error) {
	if poolSize <= 0 {
		poolSize = defaultPoolSize
	}
	if !bridgeLayoutReady || maxVrtSlotsSize == invaildSlotSize {
		return 0, fmt.Errorf("bridge layout capacity is not initialized")
	}
	if poolSize > maxVrtSlotsSize {
		return 0, fmt.Errorf("invalid network.pool_size=%d, exceeds max available slots=%d", poolSize, maxVrtSlotsSize)
	}
	return poolSize, nil
}

func NewPool(poolSize int, dynamicReservation bool, bridgeCount int, tapIP string, tapMask int) (*Pool, error) {
	if err := initConfigureBridgeLayout(bridgeCount); err != nil {
		return nil, fmt.Errorf("invalid bridge layout: %w", err)
	}
	poolSize, err := normalizeAndValidatePoolSize(poolSize)
	if err != nil {
		return nil, err
	}
	if err := configureTapNetwork(tapIP, tapMask); err != nil {
		return nil, fmt.Errorf("invalid tap network config: %w", err)
	}
	newSlots := make(chan *Slot, poolSize)

	slotStorage, err := NewStorage(maxVrtSlotsSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create new storage: %w", err)
	}

	if err := initAllBridges(); err != nil {
		return nil, fmt.Errorf("failed to init bridges: %w", err)
	}
	if err := initHostMasquerade(); err != nil {
		_ = deleteAllBridges()
		return nil, fmt.Errorf("failed to init host masquerade: %w", err)
	}

	p := &Pool{
		slotStorage:        slotStorage,
		newSlots:           newSlots,
		done:               make(chan struct{}),
		prefillReady:       make(chan struct{}),
		dynamicReservation: dynamicReservation,
		inUse:              make(map[string]*Slot),
	}

	return p, nil
}

func (p *Pool) createNetworkSlot(ctx context.Context) (*Slot, error) {
	ips, err := p.slotStorage.Acquire(ctx)
	if err != nil {
		if isExpectedShutdownError(ctx, err) {
			return nil, context.Canceled
		}
		getLogger().Error("failed to acquire network slot", ulog.F("error", err))
		return nil, fmt.Errorf("failed to acquire network slot: %w", err)
	}

	err = ips.CreateNetwork()
	if err != nil {
		releaseErr := p.slotStorage.Release(ips)
		err = errors.Join(err, releaseErr)
		if isExpectedShutdownError(ctx, err) {
			getLogger().Debug("network creation interrupted during shutdown", ulog.F("error", err))
			return nil, context.Canceled
		}
		getLogger().Error("failed to create network", ulog.F("slot_index", ips.Idx), ulog.F("error", err))
		return nil, fmt.Errorf("failed to create network for slot %d: %w", ips.Idx, err)
	}
	return ips, nil
}

func isExpectedShutdownError(ctx context.Context, err error) bool {
	// Without an error or an active context cancellation, this is not shutdown-related noise.
	if err == nil || ctx.Err() == nil {
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

func (p *Pool) Populate(ctx context.Context) {
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

	for {
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
				getLogger().Debug("pool: failed to create network", ulog.F("error", err))
				continue
			}
			p.newSlots <- slot
		}
	}
}

func (p *Pool) populateStatic(ctx context.Context) error {
	target := cap(p.newSlots)

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

	// Start a bounded worker pool to avoid overwhelming netns/iptables operations.
	for w := 0; w < workers; w++ {
		go func() {
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
						return
					case <-p.done:
						return
					case results <- result{slot: slot, err: err}:
					}
				}
			}
		}()
	}

	// Submit one prefill task per target slot; static reservation is one-shot.
	for i := 0; i < target; i++ {
		jobs <- job{}
	}
	close(jobs)

	var firstErr error
	// Drain all worker results to avoid blocked goroutine sends, then return first error if any.
	for i := 0; i < target; i++ {
		select {
		case <-p.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case r := <-results:
			if r.err != nil {
				if firstErr == nil {
					firstErr = r.err
				}
				continue
			}
			p.newSlots <- r.slot
		}
	}

	if firstErr != nil {
		return fmt.Errorf(
			"static reservation stopped before reaching target: current=%d target=%d: %w",
			len(p.newSlots), target, firstErr,
		)
	}

	getLogger().Info("pool: static reservation completed", ulog.F("acquired_total", len(p.newSlots)), ulog.F("in_pool", len(p.newSlots)), ulog.F("target", target))
	return nil
}

func (p *Pool) Get(ctx context.Context) (*Slot, error) {
	if !p.dynamicReservation {
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case s, ok := <-p.newSlots:
				if !ok {
					return nil, fmt.Errorf("network channel has been closed")
				}
				if s != nil {
					p.trackInUse(s)
				}
				return s, nil
			default:
				if p.isPrefillReady() {
					getLogger().Warn(
						"no available network slot in the pool",
						ulog.F("capacity", cap(p.newSlots)),
						ulog.F("in_use", p.inUseCount()),
						ulog.F("available", len(p.newSlots)),
					)
					return nil, fmt.Errorf("no available network slot in the pool, pool_size=%d", cap(p.newSlots))
				}
				s, ok, err := p.waitForSlotOrPrefillReady(ctx)
				if err != nil {
					return nil, err
				}
				if ok {
					if s != nil {
						p.trackInUse(s)
					}
					return s, nil
				}
			}
		}
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s, ok := <-p.newSlots:
		if !ok {
			return nil, fmt.Errorf("network channel has been closed")
		}
		if s != nil {
			p.trackInUse(s)
		}
		return s, nil
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

func (p *Pool) waitForSlotOrPrefillReady(ctx context.Context) (*Slot, bool, error) {
	select {
	case <-p.prefillReady:
		return nil, false, nil
	case s, ok := <-p.newSlots:
		if !ok {
			return nil, false, fmt.Errorf("network channel has been closed")
		}
		return s, true, nil
	case <-p.done:
		return nil, false, fmt.Errorf("pool is stopping")
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
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
	}
}

func (p *Pool) Release(ctx context.Context, slot *Slot) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		if slot != nil {
			slotHealthErr := slotHealth(slot)
			if !p.dynamicReservation && slotHealthErr == nil {
				p.untrackInUse(slot)
				if err := p.enqueueReplacement(ctx, slot); err != nil {
					p.trackInUse(slot)
					return fmt.Errorf("failed to enqueue replenished slot: %w", err)
				}
				getLogger().Info("network slot released", ulog.F("slot_index", slot.Idx))
			} else {
				if !p.dynamicReservation {
					getLogger().Warn("slot unhealthy, dropping from the static pool", ulog.F("slot", slot.Key), ulog.F("slot_index", slot.Idx), ulog.F("error", slotHealthErr))
				}
				err := slot.RemoveNetwork()
				if err != nil {
					getLogger().Error("failed to remove network", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
					return fmt.Errorf("failed to remove network for slot %d: %w", slot.Idx, err)
				}
				err = p.slotStorage.Release(slot)
				if err != nil {
					getLogger().Error("failed to release network slot", ulog.F("slot_index", slot.Idx), ulog.F("error", err))
					return fmt.Errorf("failed to release network slot %d: %w", slot.Idx, err)
				}
				p.untrackInUse(slot)
			}
		}
		return nil
	}
}

func (p *Pool) trackInUse(slot *Slot) {
	p.inUseMu.Lock()
	defer p.inUseMu.Unlock()
	p.inUse[slot.Key] = slot
}

func (p *Pool) untrackInUse(slot *Slot) {
	p.inUseMu.Lock()
	defer p.inUseMu.Unlock()
	delete(p.inUse, slot.Key)
}

func (p *Pool) inUseCount() int {
	p.inUseMu.Lock()
	defer p.inUseMu.Unlock()
	return len(p.inUse)
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

func slotHealth(slot *Slot) error {
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	if _, err := os.Stat(filepath.Join("/var/run/netns", slot.NamespaceID())); err != nil {
		return fmt.Errorf("namespace missing: %w", err)
	}
	veth, err := netlink.LinkByName(slot.VethName())
	if err != nil {
		return fmt.Errorf("veth missing: %w", err)
	}
	if veth.Attrs() == nil || veth.Attrs().MasterIndex == 0 {
		return fmt.Errorf("veth is not attached to bridge")
	}
	bridge, err := netlink.LinkByName(slot.BridgeName())
	if err != nil {
		return fmt.Errorf("bridge missing: %w", err)
	}
	if veth.Attrs().MasterIndex != bridge.Attrs().Index {
		return fmt.Errorf("veth is attached to unexpected bridge")
	}
	return nil
}

func (p *Pool) Cleanup() error {
	var errs []error
	cleaned := 0
	failed := 0
	cleanupSlot := func(slot *Slot, category string) {
		if slot == nil {
			failed++
			return
		}
		err := slot.RemoveNetwork()
		if err != nil {
			getLogger().Error("cleanup slot failed when removing network", ulog.F("slot", slot.Key), ulog.F("category", category), ulog.F("error", err))
			errs = append(errs, fmt.Errorf("cleanup slot %s failed, %w", slot.Key, err))
			failed++
			return
		}
		err = p.slotStorage.Release(slot)
		if err != nil {
			getLogger().Error("cleanup slot failed when releasing", ulog.F("slot", slot.Key), ulog.F("category", category), ulog.F("error", err))
			errs = append(errs, fmt.Errorf("cleanup slot %s failed, %w", slot.Key, err))
			failed++
			return
		}
		cleaned++
	}

	for _, slot := range p.drainInUse() {
		cleanupSlot(slot, "in_use")
	}

	for slot := range p.newSlots {
		cleanupSlot(slot, "queue")
	}

	getLogger().Info("pool cleanup summary", ulog.F("cleaned_slots", cleaned), ulog.F("failed_slots", failed))

	if err := deleteHostMasquerade(); err != nil {
		errs = append(errs, err)
	}
	if err := deleteAllBridges(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
