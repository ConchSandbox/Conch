package conchruntime

import (
	"context"
	"fmt"
	"strings"

	"github.com/openeuler/Conch/internal/sandbox"
)

func (s *Service) getSandbox(ctx context.Context, id string) (sandbox.Record, error) {
	if s == nil || s.Store == nil {
		return sandbox.Record{}, fmt.Errorf("sandbox state store is not configured")
	}
	return s.Store.Get(ctx, id)
}

func (s *Service) GetSandbox(ctx context.Context, sandboxID string) (sandbox.Record, error) {
	return s.getSandbox(ctx, sandboxID)
}

func (s *Service) ListSandboxes(ctx context.Context) ([]sandbox.Record, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("sandbox state store is not configured")
	}
	return s.Store.List(ctx, sandbox.Filter{})
}

// RemoveSandbox is a passthrough: Manager serializes the deletion with the
// sandbox's lifecycle lock and owns the record and the capacity reservation.
func (s *Service) RemoveSandbox(ctx context.Context, sandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	return s.Sandbox.Delete(ctx, sandboxID)
}

// CreateCounts reports cumulative Node lifecycle outcomes to the Scheduler.
func (s *Service) CreateCounts() (uint64, uint64) {
	return s.createSuccesses.Load(), s.createFailures.Load()
}
