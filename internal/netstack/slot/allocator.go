package slot

import (
	"container/heap"
	"errors"
	"fmt"
	"sync"
)

var ErrCapacity = errors.New("network slot capacity reached")

type idHeap []int

func (h idHeap) Len() int           { return len(h) }
func (h idHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h idHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *idHeap) Push(value any) {
	*h = append(*h, value.(int))
}

func (h *idHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}

// Allocator owns the in-memory Slot ID allocation policy. The heap returns
// the lowest free ID while the bitmap prevents duplicate releases.
type Allocator struct {
	mu      sync.Mutex
	firstID int
	used    []bool
	free    idHeap
}

func NewAllocator(firstID, capacity int) *Allocator {
	free := make(idHeap, capacity)
	for offset := range capacity {
		free[offset] = firstID + offset
	}
	heap.Init(&free)
	return &Allocator{
		firstID: firstID,
		used:    make([]bool, capacity),
		free:    free,
	}
}

// Rebuild replaces allocation state using IDs found in persistent storage.
// It must run before concurrent calls to Acquire or Release begin.
func (a *Allocator) Rebuild(usedIDs []int) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	used := make([]bool, len(a.used))
	for _, id := range usedIDs {
		offset := id - a.firstID
		if offset < 0 || offset >= len(used) {
			return fmt.Errorf("slot ID %d is outside allocator range [%d, %d)", id, a.firstID, a.firstID+len(used))
		}
		if used[offset] {
			return fmt.Errorf("slot ID %d appears more than once", id)
		}
		used[offset] = true
	}

	free := make(idHeap, 0, len(used)-len(usedIDs))
	for offset, inUse := range used {
		if !inUse {
			free = append(free, a.firstID+offset)
		}
	}
	heap.Init(&free)
	a.used = used
	a.free = free
	return nil
}

func (a *Allocator) Acquire() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.free) == 0 {
		return 0, ErrCapacity
	}
	id := heap.Pop(&a.free).(int)
	a.used[id-a.firstID] = true
	return id, nil
}

func (a *Allocator) Release(id int) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	offset := id - a.firstID
	if offset < 0 || offset >= len(a.used) {
		return fmt.Errorf("slot ID %d is outside allocator range [%d, %d)", id, a.firstID, a.firstID+len(a.used))
	}
	if !a.used[offset] {
		return fmt.Errorf("slot ID %d is already free", id)
	}
	a.used[offset] = false
	heap.Push(&a.free, id)
	return nil
}
