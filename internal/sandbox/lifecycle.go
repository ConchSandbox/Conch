package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/webhook"
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

// Delete retains the record and its GC references until resource cleanup has
// succeeded. It also handles persisted sandboxes without a live runtime entry.
func (m *Manager) Delete(parent context.Context, sandboxID string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	unlock := m.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
	defer cleanupCancel()
	if value, ok := m.sandboxes.Load(sandboxID); ok {
		entry := value.(*sandboxEntry)
		if err := entry.cleanup.Run(cleanupCtx); err != nil {
			entry.state = StateUnknown
			rec.State, rec.LastError = StateUnknown, err.Error()
			_, saveErr := m.store.Update(cleanupCtx, rec)
			return combineOperationErrors(err, saveErr)
		}
		m.sandboxes.Delete(sandboxID)
	} else if rec.ID != "" {
		if err := m.boot.Release(cleanupCtx, ReleaseBootRequest{SandboxID: sandboxID}); err != nil {
			return err
		}
	}
	if err := m.releaseCreateLease(cleanupCtx, sandboxID); err != nil {
		return err
	}
	if err := m.store.Delete(cleanupCtx, sandboxID); err != nil {
		return err
	}
	if rec.ID != "" {
		m.publishLifecycleEvent(webhook.EventSandboxKilled, rec, "request")
	}
	return nil
}

func (m *Manager) Suspend(ctx context.Context, sandboxID string) error {
	return m.changeState(ctx, sandboxID, StateReady, StateSuspended)
}

func (m *Manager) Resume(ctx context.Context, sandboxID string) error {
	return m.changeState(ctx, sandboxID, StateSuspended, StateReady)
}

func (m *Manager) changeState(parent context.Context, sandboxID string, from, to State) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	unlock := m.lifecycleLocks.lock(sandboxID)
	defer unlock()
	entry, err := m.loadSandboxEntry(sandboxID)
	if err != nil {
		return err
	}
	if entry.state != from {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, entry.state))
	}
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil {
		return err
	}
	if to == StateSuspended {
		err = entry.sbx.Suspend(ctx)
	} else {
		err = entry.sbx.Resume(ctx)
	}
	if err == nil {
		entry.state = to
	} else {
		entry.state = StateUnknown
	}
	rec.State = entry.state
	rec.LastError = ""
	if err != nil {
		rec.LastError = err.Error()
	}
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
	defer saveCancel()
	_, saveErr := m.store.Update(saveCtx, rec)
	return combineOperationErrors(err, saveErr)
}

func (m *Manager) UpdateNetwork(parent context.Context, req NetworkUpdateRequest) error {
	req.SandboxID = strings.TrimSpace(req.SandboxID)
	if req.SandboxID == "" {
		return ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	if err := netstack.ValidateSandboxNetworkInputConfig(ctx, req.Network); err != nil {
		return err
	}
	unlock := m.lifecycleLocks.lock(req.SandboxID)
	defer unlock()
	entry, err := m.loadSandboxEntry(req.SandboxID)
	if err != nil {
		return err
	}
	if entry.state != StateReady && entry.state != StateSuspended {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state))
	}
	rec, err := m.store.Get(ctx, req.SandboxID)
	if err != nil {
		return err
	}
	oldNetwork := rec.Network
	rec.Network, rec.LastError = req.Network, ""
	if _, err := m.store.Update(ctx, rec); err != nil {
		return err
	}
	if err := m.pool.SetSandboxNetworkPolicy(ctx, entry.sbx.slot, req.SandboxID, req.Network); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
		defer cancel()
		rollbackErr := m.pool.SetSandboxNetworkPolicy(cleanupCtx, entry.sbx.slot, req.SandboxID, oldNetwork)
		if rollbackErr != nil {
			if entry.state == StateReady {
				rollbackErr = errors.Join(rollbackErr, entry.sbx.Suspend(cleanupCtx))
			}
			entry.state = StateUnknown
		}
		rec.State, rec.Network = entry.state, oldNetwork
		rec.LastError = errors.Join(err, rollbackErr).Error()
		_, saveErr := m.store.Update(cleanupCtx, rec)
		return combineOperationErrors(err, rollbackErr, saveErr)
	}
	return nil
}

func (m *Manager) handleSandboxExit(sandboxID string, entry *sandboxEntry) {
	unlock := m.lifecycleLocks.lock(sandboxID)
	defer unlock()
	current, ok := m.sandboxes.Load(sandboxID)
	if !ok || current != entry {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createCleanupTimeout)
	defer cancel()
	cleanupErr := entry.cleanup.Run(ctx)
	if cleanupErr == nil {
		m.sandboxes.Delete(sandboxID)
	} else {
		entry.state = StateUnknown
	}
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil {
		ulog.GetLogger().Error("read exited sandbox", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
		return
	}
	alreadyExited := rec.State == StateUnknown
	rec.State = StateUnknown
	if cleanupErr != nil {
		rec.LastError = cleanupErr.Error()
	} else {
		rec.LastError = ""
		rec.RuntimeSnapshots = nil
	}
	if _, err := m.store.Update(ctx, rec); err != nil {
		ulog.GetLogger().Error("persist exited sandbox", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
		return
	}
	if !alreadyExited {
		m.publishLifecycleEvent(webhook.EventSandboxKilled, rec, "orphaned")
	}
}

func (m *Manager) publishLifecycleEvent(eventType string, rec Record, killReason string) {
	if m.WebhookDispatcher == nil {
		return
	}
	event, err := webhook.NewEvent(eventType, rec.ID, killReason, webhook.Execution{
		CreatedAt: time.Unix(0, rec.CreatedAt).UTC().Format(time.RFC3339), VCPUNum: rec.VCPUNum, RamMB: rec.RamMB,
	})
	if err != nil {
		ulog.GetLogger().Error("create sandbox lifecycle event", ulog.F("sandbox_id", rec.ID), ulog.F("error", err))
		return
	}
	m.WebhookDispatcher.Publish(event)
}
