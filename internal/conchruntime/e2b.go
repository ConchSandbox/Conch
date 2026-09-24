package conchruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/pkg/ulog"
)

type sandboxLifecycleLock struct {
	mu   sync.Mutex
	refs int
}

type sandboxLifecycleLocks struct {
	mu      sync.Mutex
	entries map[string]*sandboxLifecycleLock
}

func (l *sandboxLifecycleLocks) lock(id string) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*sandboxLifecycleLock)
	}
	entry := l.entries[id]
	if entry == nil {
		entry = &sandboxLifecycleLock{}
		l.entries[id] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 && l.entries[id] == entry {
			delete(l.entries, id)
		}
		l.mu.Unlock()
	}
}

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

// RemoveSandbox serializes lifecycle transitions per public ID. A record that
// no longer exists is not an error: Manager.Delete already cleaned it up.
func (s *Service) RemoveSandbox(ctx context.Context, sandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	defer unlock()
	var rec sandbox.Record
	if s.Store != nil {
		var getErr error
		rec, getErr = s.getSandbox(ctx, sandboxID)
		if getErr != nil && !errors.Is(getErr, sandbox.ErrNotFound) {
			return getErr
		}
	}
	return s.removeSandboxLocked(ctx, sandboxID, rec)
}

// ReconcileSandbox applies a maintenance observation only to the same runtime
// and only if it is still eligible. A public ID can be reused after DELETE.
func (s *Service) ReconcileSandbox(ctx context.Context, sandboxID, runtimeID string, observedAt time.Time) error {
	if s == nil || s.Sandbox == nil || s.Store == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	if runtimeID == "" {
		return nil
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := s.getSandbox(ctx, sandboxID)
	if errors.Is(err, sandbox.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if rec.RuntimeID != runtimeID {
		return nil
	}
	// Manager retains the record after an unconfirmed cleanup; an expired E2B
	// deadline is the other authorized reason to remove a live observation.
	eligible := rec.State == sandbox.StateUnknown ||
		(rec.E2B && rec.ExpiresAt > 0 && rec.ExpiresAt <= observedAt.UnixNano())
	if !eligible {
		return nil
	}
	return s.removeSandboxLocked(ctx, sandboxID, rec)
}

// removeSandboxLocked requires lifecycleLocks for this ID. Manager.Delete
// owns the persistent record, so it deletes it after a confirmed cleanup and
// retains it (UNKNOWN) otherwise; the capacity reservation follows the same
// owner to avoid stranding or refunding it prematurely.
func (s *Service) removeSandboxLocked(ctx context.Context, sandboxID string, rec sandbox.Record) error {
	s.removeProxyRoute(sandboxID)
	cleanupErr := s.Sandbox.Delete(ctx, sandboxID)
	if errors.Is(cleanupErr, sandbox.ErrNotFound) {
		cleanupErr = nil
	}
	if errors.Is(cleanupErr, sandbox.ErrFailedPrecondition) {
		return cleanupErr
	}
	if cleanupErr != nil {
		// Manager retained the record; it still owns the reservation.
		return cleanupErr
	}
	s.Capacity.release(sandboxID)
	return nil
}

// HandleSandboxUnexpectedExit observes an asynchronous runtime exit. Manager
// has already cleaned up and persisted the UNKNOWN record; the service only
// retires its data-plane route and, when release is confirmed, the capacity
// reservation.
func (s *Service) HandleSandboxUnexpectedExit(sandboxID, runtimeID string, cleanupErr error) {
	if s == nil || s.Store == nil {
		return
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := s.getSandbox(context.Background(), sandboxID)
	if errors.Is(err, sandbox.ErrNotFound) {
		return
	}
	if err != nil {
		ulog.GetLogger().Error("failed to read sandbox after unexpected exit", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
		return
	}
	// Manager may already have removed the old entry when this callback
	// acquires lifecycleLocks. Native clients can reuse the public ID after
	// deletion, so only the matching runtime may retire its current resources.
	if runtimeID == "" || rec.RuntimeID != runtimeID {
		return
	}
	s.removeProxyRoute(sandboxID)
	if cleanupErr == nil {
		s.Capacity.release(sandboxID)
	}
}

func (s *Service) removeProxyRoute(sandboxID string) {
	if s.ProxyRoutes != nil {
		if generation, ok := s.ProxyRoutes.CurrentGeneration(sandboxID); ok {
			s.ProxyRoutes.Remove(sandboxID, generation)
		}
	}
}

// HandleSandboxRuntimeExiting runs while the Manager still owns its current
// runtime entry, before its interaction IP can return to the network pool.
// Do not take lifecycleLocks here: Manager holds its own per-sandbox lock.
func (s *Service) HandleSandboxRuntimeExiting(sandboxID string) {
	s.removeProxyRoute(sandboxID)
}

// LockSandboxRoster serializes membership changes with the complete heartbeat
// RPC. A roster captured before Create cannot arrive after its assignment and
// delete that new binding in AgentENV's authoritative reconciliation.
func (s *Service) LockSandboxRoster() func() {
	s.rosterMu.Lock()
	return s.rosterMu.Unlock
}

// CreateCounts reports cumulative Node lifecycle outcomes to the Scheduler.
func (s *Service) CreateCounts() (uint64, uint64) {
	return s.createSuccesses.Load(), s.createFailures.Load()
}

// combineOperationErrors keeps the primary operation's application
// classification when cleanup also fails; secondary errors must not turn an
// application error into a different class.
func combineOperationErrors(primary error, secondary error) error {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	return errors.Join(primary, secondary)
}
