package sandbox

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

func TestCIDAllocatorLifecycle(t *testing.T) {
	allocator := NewCIDAllocator()

	first, err := allocator.AllocateCID("sandbox-a")
	if err != nil {
		t.Fatalf("AllocateCID(sandbox-a) error = %v", err)
	}
	if first != MinCID {
		t.Fatalf("AllocateCID(sandbox-a) = %d, want %d", first, MinCID)
	}

	again, err := allocator.AllocateCID("sandbox-a")
	if err != nil {
		t.Fatalf("AllocateCID(sandbox-a) again error = %v", err)
	}
	if again != first {
		t.Fatalf("AllocateCID(sandbox-a) again = %d, want %d", again, first)
	}

	second, err := allocator.AllocateCID("sandbox-b")
	if err != nil {
		t.Fatalf("AllocateCID(sandbox-b) error = %v", err)
	}
	if second != first+1 {
		t.Fatalf("AllocateCID(sandbox-b) = %d, want %d", second, first+1)
	}

	if err := allocator.ReleaseCID("sandbox-a"); err != nil {
		t.Fatalf("ReleaseCID(sandbox-a) error = %v", err)
	}
	if _, ok := allocator.GetCID("sandbox-a"); ok {
		t.Fatal("GetCID(sandbox-a) found released CID")
	}

	third, err := allocator.AllocateCID("sandbox-c")
	if err != nil {
		t.Fatalf("AllocateCID(sandbox-c) error = %v", err)
	}
	if third != second+1 {
		t.Fatalf("AllocateCID(sandbox-c) = %d, want monotonic CID %d", third, second+1)
	}

	if got := allocator.GetActiveCount(); got != 2 {
		t.Fatalf("GetActiveCount() = %d, want 2", got)
	}
	active := allocator.ListActiveSandboxes()
	sort.Strings(active)
	if len(active) != 2 || active[0] != "sandbox-b" || active[1] != "sandbox-c" {
		t.Fatalf("ListActiveSandboxes() = %v, want [sandbox-b sandbox-c]", active)
	}
}

func TestCIDAllocatorConcurrentAllocation(t *testing.T) {
	const sandboxCount = 128
	allocator := NewCIDAllocator()

	var wg sync.WaitGroup
	results := make(chan uint32, sandboxCount)
	errs := make(chan error, sandboxCount)
	for i := range sandboxCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cid, err := allocator.AllocateCID(fmt.Sprintf("sandbox-%d", i))
			if err != nil {
				errs <- err
				return
			}
			results <- cid
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("AllocateCID() error = %v", err)
	}
	seen := make(map[uint32]struct{}, sandboxCount)
	for cid := range results {
		if _, ok := seen[cid]; ok {
			t.Errorf("CID %d allocated more than once", cid)
		}
		seen[cid] = struct{}{}
	}
	if len(seen) != sandboxCount {
		t.Fatalf("allocated %d unique CIDs, want %d", len(seen), sandboxCount)
	}
}

func TestCIDAllocatorExhaustion(t *testing.T) {
	allocator := NewCIDAllocator()
	allocator.nextCID = MaxCID

	cid, err := allocator.AllocateCID("last-sandbox")
	if err != nil {
		t.Fatalf("AllocateCID(last-sandbox) error = %v", err)
	}
	if cid != MaxCID {
		t.Fatalf("AllocateCID(last-sandbox) = %d, want %d", cid, MaxCID)
	}

	if _, err := allocator.AllocateCID("overflow-sandbox"); err == nil {
		t.Fatal("AllocateCID(overflow-sandbox) error = nil, want exhaustion error")
	}
}

func TestCIDAllocatorStateIsProcessLocal(t *testing.T) {
	first := NewCIDAllocator()
	if _, err := first.AllocateCID("sandbox-a"); err != nil {
		t.Fatalf("first AllocateCID(sandbox-a) error = %v", err)
	}

	second := NewCIDAllocator()
	cid, err := second.AllocateCID("sandbox-b")
	if err != nil {
		t.Fatalf("second AllocateCID(sandbox-b) error = %v", err)
	}
	if cid != MinCID {
		t.Fatalf("second allocator first CID = %d, want %d", cid, MinCID)
	}
}
