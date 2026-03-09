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

	"github.com/openeuler/Conch/pkg/ulog"
	"github.com/vishvananda/netlink"
)

const (
	defaultPoolSize = 250
	cleanupTimeout  = 10
	bridgeName      = "conch_bridge"
)

type Pool struct {
	slotStorage        Storage
	newSlots           chan *Slot
	done               chan struct{}
	dynamicReservation bool
}

func initBridge() (retErr error) {
	// Create hostns bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeName,
			MTU:  1500,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		ulog.Error("failed to create bridge", ulog.F("error", err))
		return fmt.Errorf("error create bridge: %w", err)
	}
	defer func() {
		if retErr != nil {
			deleteBridge()
		}
	}()

	bridgeLink, err := netlink.LinkByName(bridgeName)
	if err != nil {
		ulog.Error("failed to find bridge", ulog.F("bridge", bridgeName), ulog.F("error", err))
		return fmt.Errorf("error finding bridge %s: %w", bridgeName, err)
	}
	err = netlink.LinkSetUp(bridgeLink)
	if err != nil {
		ulog.Error("failed to set vpeer device up", ulog.F("error", err))
		return fmt.Errorf("error setting vpeer device up: %w", err)
	}

	return nil
}

func deleteBridge() error {
	veth, err := netlink.LinkByName(bridgeName)
	if err != nil {
		ulog.Error("failed to find bridge", ulog.F("error", err))
		return fmt.Errorf("error finding bridge: %w", err)
	}
	err = netlink.LinkDel(veth)
	if err != nil {
		ulog.Error("failed to delete bridge device", ulog.F("error", err))
		return fmt.Errorf("error delete bridge device: %w", err)
	}
	return nil
}

func NewPool(poolSize int, dynamicReservation bool) (*Pool, error) {
	if poolSize <= 0 {
		poolSize = defaultPoolSize
	}

	if poolSize > maxVrtSlotsSize {
		return nil, fmt.Errorf("invalid network.pool_size=%d, exceeds max available slots=%d", poolSize, maxVrtSlotsSize)
	}
	newSlots := make(chan *Slot, poolSize)

	slotStorage, err := NewStorage(maxVrtSlotsSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create new storage: %w", err)
	}

	if err := initBridge(); err != nil {
		return nil, fmt.Errorf("failed to init bridge: %w", err)
	}

	p := &Pool{
		slotStorage:        slotStorage,
		newSlots:           newSlots,
		done:               make(chan struct{}),
		dynamicReservation: dynamicReservation,
	}

	return p, nil
}

func (p *Pool) createNetworkSlot(ctx context.Context) (*Slot, error) {
	ips, err := p.slotStorage.Acquire(ctx)
	if err != nil {
		ulog.Error("failed to acquire network slot", ulog.F("error", err))
		return nil, fmt.Errorf("failed to acquire network slot: %w", err)
	}

	err = ips.CreateNetwork()
	if err != nil {
		releaseErr := p.slotStorage.Release(ips)
		err = errors.Join(err, releaseErr)
		ulog.Error("failed to create network", ulog.F("error", err))
		return nil, fmt.Errorf("failed to create network: %w", err)
	}
	return ips, nil
}

func (p *Pool) Populate(ctx context.Context) {
	defer close(p.newSlots)

	if !p.dynamicReservation {
		p.populateStatic(ctx)
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
				ulog.Debug("pool: failed to create network", ulog.F("error", err))
				continue
			}
			p.newSlots <- slot
		}
	}
}

func (p *Pool) populateStatic(ctx context.Context) {
	target := cap(p.newSlots)
	for len(p.newSlots) < target {
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			return
		default:
			slot, err := p.createNetworkSlot(ctx)
			if err != nil {
				ulog.Warn("pool: static reservation stopped before reaching target", ulog.F("current", len(p.newSlots)), ulog.F("target", target), ulog.F("error", err))
				return
			}
			p.newSlots <- slot
		}
	}
	ulog.Info("pool: static reservation completed", ulog.F("reserved", len(p.newSlots)))
}

func (p *Pool) Get(ctx context.Context) (*Slot, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s := <-p.newSlots:
		return s, nil
	}
}

func (p *Pool) enqueueReplacement(ctx context.Context, slot *Slot) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// Populate closes newSlots during shutdown; Release may race with that close.
			err = fmt.Errorf("network pool is closed")
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
			if p.dynamicReservation {
				err := slot.RemoveNetwork()
				if err != nil {
					ulog.Error("failed to remove network", ulog.F("error", err))
					return fmt.Errorf("failed to remove network: %w", err)
				}
				err = p.slotStorage.Release(slot)
				if err != nil {
					ulog.Error("failed to release network slot", ulog.F("error", err))
					return fmt.Errorf("failed to release network slot: %w", err)
				}
			} else {
				if err := p.enqueueReplacement(ctx, slot); err != nil {
					return fmt.Errorf("failed to enqueue replenished slot: %w", err)
				}
			}
		}
		return nil
	}
}

func (p *Pool) Cleanup() error {
	var errs []error
	for slot := range p.newSlots {
		if slot != nil {
			err := slot.RemoveNetwork()
			if err != nil {
				ulog.Error("cleanup slot failed when removing network", ulog.F("slot", slot.Key), ulog.F("error", err))
				errs = append(errs, fmt.Errorf("cleanup slot %s failed, %w", slot.Key, err))
				continue
			}
			err = p.slotStorage.Release(slot)
			if err != nil {
				ulog.Error("cleanup slot failed when releasing", ulog.F("slot", slot.Key), ulog.F("error", err))
				errs = append(errs, fmt.Errorf("cleanup slot %s failed, %w", slot.Key, err))
				continue
			}
		}
	}
	// del bridge
	errs = append(errs, deleteBridge())
	return errors.Join(errs...)
}
