package conchruntime

import (
	"fmt"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/sandbox"
)

// CapacityLimits bounds running and starting sandboxes together. Zero CPU or
// memory limits select the host's capacity; positive values allow explicit
// overcommit. Reservations are held until runtime teardown completes.
type CapacityLimits struct {
	MaxSandboxes int64
	MaxCPUs      int64
	MaxMemoryMB  int64
}

type allocation struct{ cpus, memoryMB int64 }

type Capacity struct {
	mu           sync.Mutex
	limits       CapacityLimits
	used         allocation
	reservations map[string]allocation
}

func NewCapacity(limits CapacityLimits) (*Capacity, error) {
	if limits.MaxSandboxes <= 0 || limits.MaxCPUs < 0 || limits.MaxMemoryMB < 0 {
		return nil, fmt.Errorf("invalid sandbox capacity limits")
	}
	if limits.MaxCPUs == 0 {
		limits.MaxCPUs = int64(runtime.NumCPU())
	}
	if limits.MaxMemoryMB == 0 {
		var info unix.Sysinfo_t
		if err := unix.Sysinfo(&info); err != nil {
			return nil, fmt.Errorf("read host memory capacity: %w", err)
		}
		limits.MaxMemoryMB = int64(uint64(info.Totalram) * uint64(info.Unit) / (1 << 20))
	}
	return &Capacity{limits: limits, reservations: make(map[string]allocation)}, nil
}

func (c *Capacity) reserve(id string, cpus, memoryMB int64) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.reservations[id]; exists {
		return sandbox.ErrAlreadyExists.Wrap(fmt.Errorf("sandbox %s already reserves capacity", id))
	}
	if cpus <= 0 || memoryMB <= 0 || int64(len(c.reservations)) >= c.limits.MaxSandboxes ||
		cpus > c.limits.MaxCPUs-c.used.cpus || memoryMB > c.limits.MaxMemoryMB-c.used.memoryMB {
		return sandbox.ErrResourceExhausted.Wrap(fmt.Errorf("node capacity exceeded"))
	}
	c.reservations[id] = allocation{cpus, memoryMB}
	c.used.cpus += cpus
	c.used.memoryMB += memoryMB
	return nil
}

func (c *Capacity) release(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if reserved, ok := c.reservations[id]; ok {
		c.used.cpus -= reserved.cpus
		c.used.memoryMB -= reserved.memoryMB
		delete(c.reservations, id)
	}
}
