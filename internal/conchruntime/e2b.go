package conchruntime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
)

func (s *Service) GetSandbox(ctx context.Context, sandboxID string) (sandbox.Record, error) {
	if s == nil || s.Store == nil {
		return sandbox.Record{}, fmt.Errorf("sandbox state store is not configured")
	}
	return s.Store.Get(ctx, sandboxID)
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

// PauseSandbox checkpoints a running E2B sandbox and releases its runtime.
func (s *Service) PauseSandbox(ctx context.Context, sandboxID string, expiredBefore time.Time) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	return s.Sandbox.Pause(ctx, sandboxID, expiredBefore)
}

// ResumePausedSandbox boots a PAUSED sandbox from its checkpoint head. The
// requested VMM comes from the node defaults; the checkpoint boot index
// decides the actual one.
func (s *Service) ResumePausedSandbox(ctx context.Context, sandboxID string, opts sandbox.ResumePausedOptions) (runtimeapi.SandboxCreateResult, error) {
	if s == nil || s.Sandbox == nil {
		return runtimeapi.SandboxCreateResult{}, fmt.Errorf("sandbox service is not configured")
	}
	if strings.TrimSpace(opts.VMMName) == "" {
		opts.VMMName = s.SandboxDefaults.VMMName
	}
	return s.Sandbox.ResumePaused(ctx, sandboxID, opts)
}

// EnsureSandboxRunning makes an E2B sandbox serve data-plane traffic. A
// running sandbox is a no-op that never touches its TTL; a paused sandbox is
// restored from its checkpoint only when it was created with autoResume,
// granted the given TTL. This backs wake-on-traffic: the proxy calls it on a
// route miss before giving up on the sandbox.
func (s *Service) EnsureSandboxRunning(ctx context.Context, sandboxID string, ttl time.Duration) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	opts := sandbox.ResumePausedOptions{
		Timeout: ttl,
		// The node default names the requested VMM; the checkpoint boot
		// index decides the actual one.
		VMMName: s.SandboxDefaults.VMMName,
	}
	return s.Sandbox.EnsureRunning(ctx, sandboxID, opts)
}

// ApplySandboxTTL applies a TTL change to a READY E2B sandbox.
func (s *Service) ApplySandboxTTL(ctx context.Context, sandboxID string, req sandbox.TTLRequest) (sandbox.Record, error) {
	if s == nil || s.Sandbox == nil {
		return sandbox.Record{}, fmt.Errorf("sandbox service is not configured")
	}
	return s.Sandbox.ApplyTTL(ctx, sandboxID, req)
}

// ExpireSandbox kills an expired E2B sandbox under the unified terminal rule.
func (s *Service) ExpireSandbox(ctx context.Context, sandboxID string, expiredBefore time.Time) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	return s.Sandbox.Expire(ctx, sandboxID, expiredBefore)
}

// CreateCounts reports cumulative Node lifecycle outcomes to the Scheduler.
func (s *Service) CreateCounts() (uint64, uint64) {
	return s.createSuccesses.Load(), s.createFailures.Load()
}
