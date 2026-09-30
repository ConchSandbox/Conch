package conchruntime

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/openeuler/Conch/internal/sandbox"
)

func TestCapacityConcurrentReservationsCannotOvercommit(t *testing.T) {
	c, err := NewCapacity(CapacityLimits{MaxSandboxes: 4, MaxCPUs: 8, MaxMemoryMB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	reserved := make(chan string, 64)
	var wg sync.WaitGroup
	for index := range 64 {
		wg.Go(func() {
			<-start
			id := fmt.Sprintf("sandbox-%d", index)
			if err := c.reserve(id, 2, 256); err == nil {
				reserved <- id
			} else if !errors.Is(err, sandbox.ErrResourceExhausted) {
				t.Errorf("reserve %s: %v", id, err)
			}
		})
	}
	close(start)
	wg.Wait()
	close(reserved)
	if len(reserved) != 4 || len(c.reservations) != 4 || c.used.cpus != 8 || c.used.memoryMB != 1024 {
		t.Fatalf("overcommitted reservations=%d used=%+v", len(c.reservations), c.used)
	}
	for id := range reserved {
		if err := c.reserve(id, 2, 256); !errors.Is(err, sandbox.ErrAlreadyExists) {
			t.Errorf("duplicate reserve = %v", err)
		}
		c.release(id)
		c.release(id)
	}
	if len(c.reservations) != 0 || c.used != (allocation{}) {
		t.Fatalf("release leaked capacity: %+v", c.used)
	}
	if err := c.reserve("all", 8, 1024); err != nil {
		t.Fatalf("released capacity cannot be reused: %v", err)
	}
}

func TestCapacityRejectsEachResourceLimitWithoutLeaking(t *testing.T) {
	for _, limits := range []CapacityLimits{
		{MaxSandboxes: 1, MaxCPUs: 100, MaxMemoryMB: 10000},
		{MaxSandboxes: 100, MaxCPUs: 2, MaxMemoryMB: 10000},
		{MaxSandboxes: 100, MaxCPUs: 100, MaxMemoryMB: 256},
	} {
		c, err := NewCapacity(limits)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.reserve("first", 2, 256); err != nil {
			t.Fatal(err)
		}
		if err := c.reserve("second", 2, 256); !errors.Is(err, sandbox.ErrResourceExhausted) {
			t.Fatalf("limits %+v allowed overcommit: %v", limits, err)
		}
		if len(c.reservations) != 1 || c.used != (allocation{2, 256}) {
			t.Fatalf("rejection changed reservation counters: %+v", c.used)
		}
	}
}

func TestCapacityWithoutSandboxCountLimitStillEnforcesResources(t *testing.T) {
	c, err := NewCapacity(CapacityLimits{MaxCPUs: 4, MaxMemoryMB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three", "four"} {
		if err := c.reserve(id, 1, 256); err != nil {
			t.Fatalf("reserve %s: %v", id, err)
		}
	}
	if err := c.reserve("five", 1, 256); !errors.Is(err, sandbox.ErrResourceExhausted) {
		t.Fatalf("resource limit ignored without sandbox count limit: %v", err)
	}
}
