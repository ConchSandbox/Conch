package slot

import (
	"sync"
	"testing"
)

const testFirstID = 2

func TestAllocatorReturnsLowestFreeID(t *testing.T) {
	allocator := NewAllocator(testFirstID, 3)

	first, err := allocator.Acquire()
	if err != nil || first != testFirstID {
		t.Fatalf("first Acquire() = (%d, %v), want (%d, nil)", first, err, testFirstID)
	}
	second, err := allocator.Acquire()
	if err != nil || second != testFirstID+1 {
		t.Fatalf("second Acquire() = (%d, %v), want (%d, nil)", second, err, testFirstID+1)
	}
	if err := allocator.Release(first); err != nil {
		t.Fatalf("Release(%d): %v", first, err)
	}
	reused, err := allocator.Acquire()
	if err != nil || reused != first {
		t.Fatalf("reused Acquire() = (%d, %v), want (%d, nil)", reused, err, first)
	}
}

func TestAllocatorConcurrentAcquireIsUnique(t *testing.T) {
	const capacity = 128
	allocator := NewAllocator(testFirstID, capacity)
	ids := make(chan int, capacity)
	errs := make(chan error, capacity)
	var wg sync.WaitGroup
	for range capacity {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := allocator.Acquire()
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Acquire(): %v", err)
	}

	seen := make(map[int]struct{}, capacity)
	for id := range ids {
		if _, ok := seen[id]; ok {
			t.Fatalf("slot ID %d was acquired twice", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != capacity {
		t.Fatalf("unique acquired IDs = %d, want %d", len(seen), capacity)
	}
}
