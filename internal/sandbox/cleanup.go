package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

type Cleanup struct {
	cleanup         []func(ctx context.Context) error
	priorityCleanup []func(ctx context.Context) error
	err           error
	once            sync.Once

	hasRun atomic.Bool
	mu     sync.Mutex
}

func NewCleanup() *Cleanup {
	return &Cleanup{}
}

func (c *Cleanup) Add(f func(ctx context.Context) error) {
	if c.hasRun.Load() {
		zap.L().Error("Add called after cleanup has run, ignoring function")
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanup = append(c.cleanup, f)
}

func (c *Cleanup) AddPriority(f func(ctx context.Context) error) {
	if c.hasRun.Load() {
		zap.L().Error("AddPriority called after cleanup has run, ignoring function")
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.priorityCleanup = append(c.priorityCleanup, f)
}

func (c *Cleanup) Run(ctx context.Context) error {
	c.once.Do(func() {
		c.run(context.WithoutCancel(ctx))
	})
	return c.err
}

func (c *Cleanup) run(ctx context.Context) {
	c.hasRun.Store(true)

	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error

	for i := len(c.priorityCleanup) - 1; i >= 0; i-- {
		err := c.priorityCleanup[i](ctx)
		if err != nil {
			errs = append(errs, err)
		}
	}

	for i := len(c.cleanup) - 1; i >= 0; i-- {
		err := c.cleanup[i](ctx)
		if err != nil {
			errs = append(errs, err)
		}
	}

	c.err = errors.Join(errs...)
}

func cleanupFiles(files ...string) error {
	var errs []error

	for _, p := range files {
		err := os.RemoveAll(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete '%s': %w", p, err))
		}
	}

	return errors.Join(errs...)
}
