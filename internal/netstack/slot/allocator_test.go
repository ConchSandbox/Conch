package slot

import (
	"errors"
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

func TestAllocatorRebuildsFromPersistedIDs(t *testing.T) {
	allocator := NewAllocator(testFirstID, 3)
	if err := allocator.Rebuild([]int{testFirstID, testFirstID + 2}); err != nil {
		t.Fatalf("Rebuild(): %v", err)
	}

	id, err := allocator.Acquire()
	if err != nil || id != testFirstID+1 {
		t.Fatalf("Acquire() after rebuild = (%d, %v), want (%d, nil)", id, err, testFirstID+1)
	}
	if _, err := allocator.Acquire(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Acquire() at capacity error = %v, want ErrCapacity", err)
	}
	if err := allocator.Release(testFirstID); err != nil {
		t.Fatalf("Release() persisted ID: %v", err)
	}
	if err := allocator.Release(testFirstID); err == nil {
		t.Fatal("duplicate Release() succeeded")
	}
}

func TestAllocatorRejectsInvalidRebuild(t *testing.T) {
	allocator := NewAllocator(testFirstID, 2)
	if err := allocator.Rebuild([]int{testFirstID, testFirstID}); err == nil {
		t.Fatal("Rebuild() with duplicate ID succeeded")
	}
	if err := allocator.Rebuild([]int{testFirstID - 1}); err == nil {
		t.Fatal("Rebuild() with out-of-range ID succeeded")
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
