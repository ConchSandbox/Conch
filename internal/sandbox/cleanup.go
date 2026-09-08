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

[MODIFIED] - Changes made on 2025-12-27 by Team conch: cleanupFiles function
has been refined in line with the implementation of the conchd sandbox.
*/
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// Cleanup releases resources in reverse acquisition order. A failed release
// keeps that resource and its dependencies for a later retry.
type Cleanup struct {
	cleanup         []func(context.Context) error
	priorityCleanup []func(context.Context) error
	started         bool
	mu              sync.Mutex
}

func NewCleanup() *Cleanup { return &Cleanup{} }

func (c *Cleanup) Add(f func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		slog.Error("Add called after cleanup started")
		return
	}
	c.cleanup = append(c.cleanup, f)
}

func (c *Cleanup) AddPriority(f func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		slog.Error("AddPriority called after cleanup started")
		return
	}
	c.priorityCleanup = append(c.priorityCleanup, f)
}

func (c *Cleanup) Run(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
	for _, actions := range []*[]func(context.Context) error{&c.priorityCleanup, &c.cleanup} {
		for len(*actions) > 0 {
			i := len(*actions) - 1
			if err := (*actions)[i](ctx); err != nil {
				return err
			}
			*actions = (*actions)[:i]
		}
	}
	return nil
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
