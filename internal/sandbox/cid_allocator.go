package sandbox

import (
	"fmt"
	"sync"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	MinCID = 3
	MaxCID = uint32(4294967294) // uint32 max - 1, reserved to avoid the wildcard CID
)

// CIDAllocator owns process-local sandbox-to-vsock-CID assignments.
// Assignments are intentionally not persisted; callers must ensure no VMs
// from an earlier process instance remain before starting a new allocator.
type CIDAllocator struct {
	mu           sync.RWMutex
	nextCID      uint32
	cidBySandbox map[string]uint32
}

func NewCIDAllocator() *CIDAllocator {
	return &CIDAllocator{
		nextCID:      MinCID,
		cidBySandbox: make(map[string]uint32),
	}
}

func (a *CIDAllocator) AllocateCID(sandboxID string) (uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if existingCID, ok := a.cidBySandbox[sandboxID]; ok {
		return existingCID, nil
	}

	if a.nextCID > MaxCID {
		return 0, fmt.Errorf("CID allocation failed: reached maximum CID limit (%d); current active sandboxes: %d",
			MaxCID, len(a.cidBySandbox))
	}

	cid := a.nextCID
	a.cidBySandbox[sandboxID] = cid
	a.nextCID++

	ulog.Info("allocated CID", ulog.F("sandbox_id", sandboxID), ulog.F("cid", cid))
	return cid, nil
}

func (a *CIDAllocator) ReleaseCID(sandboxID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.cidBySandbox, sandboxID)
	return nil
}

func (a *CIDAllocator) GetCID(sandboxID string) (uint32, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	cid, ok := a.cidBySandbox[sandboxID]
	return cid, ok
}

func (a *CIDAllocator) GetActiveCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return len(a.cidBySandbox)
}

func (a *CIDAllocator) ListActiveSandboxes() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()

	sandboxIDs := make([]string, 0, len(a.cidBySandbox))
	for sandboxID := range a.cidBySandbox {
		sandboxIDs = append(sandboxIDs, sandboxID)
	}
	return sandboxIDs
}
