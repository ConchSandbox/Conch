package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	recordExists := err == nil
	var entry *sandboxEntry
	if value, ok := m.sandboxes.Load(sandboxID); ok {
		entry = value.(*sandboxEntry)
		entry.state = StateUnknown
	}
	if entry != nil {
		if err := m.cleanupSandbox(cleanupCtx, sandboxID, entry); err != nil {
			return m.persistDeleteFailure(cleanupCtx, rec, recordExists, err)
		}
	}
	if err := m.releaseCreateLease(cleanupCtx, sandboxID); err != nil {
		return m.persistDeleteFailure(cleanupCtx, rec, recordExists, err)
	}
	deleteErr := m.store.Delete(cleanupCtx, sandboxID)
	if deleteErr != nil {
		return m.persistDeleteFailure(cleanupCtx, rec, recordExists, deleteErr)
	}
	m.sandboxes.Delete(sandboxID)
	m.releaseCapacity(rec)

	if recordExists && (entry == nil || !entry.exitNotified) {
		m.publishLifecycleEvent(webhook.EventSandboxKilled, rec, "request")
	}
	return nil
}

func (m *Manager) persistSandboxFailure(ctx context.Context, rec Record, exists bool, err error) error {
	if !exists {
		return err
	}
	// Incomplete creates retain their recovery classification.
	if rec.State != StateCreating {
		rec.State = StateUnknown
	}
	rec.LastError = err.Error()
	_, saveErr := m.store.Update(ctx, rec)
	return combineOperationErrors(err, saveErr)
}

// persistDeleteFailure keeps the deletion intent across a failed terminal
// write: the record goes UNKNOWN with its checkpoint lineage cleared, so the
// delete retry — an explicit Delete or startup recovery — deletes it instead
// of writing it back as PAUSED.
func (m *Manager) persistDeleteFailure(ctx context.Context, rec Record, exists bool, err error) error {
	if exists {
		rec.State = StateUnknown
		rec.CheckpointHeadTemplateID = rec.SourceTemplateID
	}
	return m.persistSandboxFailure(ctx, rec, exists, err)
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
	if !ok || current != entry || entry.state == StateUnknown {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createCleanupTimeout)
	defer cancel()
	cleanupErr := m.cleanupSandbox(ctx, sandboxID, entry)
	if cleanupErr != nil {
		ulog.GetLogger().Warn("cleanup exited sandbox", ulog.F("sandbox_id", sandboxID), ulog.F("error", cleanupErr))
	}
	m.finalizeDeath(ctx, sandboxID, entry, cleanupErr)
}

// finalizationOptions carries the caller-specific parts of the unified
// terminal rule: killReason is the killed-event reason and empty suppresses
// the event, entry is the runtime entry whose notified exit suppresses a
// duplicate.
type finalizationOptions struct {
	killReason string
	entry      *sandboxEntry
	publish    bool
}

// finalizeReleased applies the unified terminal rule to a record whose
// runtime resources are confirmed released: a recoverable checkpoint lineage
// is written back as PAUSED with its head pinned, any other record is
// deleted. Both confirmed branches drop the runtime entry and the capacity
// reservation; a failed terminal write retains the record as UNKNOWN for an
// explicit delete or startup recovery, which finalize through the same rule.
func (m *Manager) finalizeReleased(ctx context.Context, rec Record, opts finalizationOptions) error {
	if m.recoverableLineage(rec) {
		if err := m.writeBackPaused(ctx, rec); err != nil {
			return m.persistSandboxFailure(ctx, rec, true, err)
		}
	} else {
		deleteErr := m.store.Delete(ctx, rec.ID)
		if deleteErr != nil {
			return m.persistSandboxFailure(ctx, rec, true, deleteErr)
		}
	}
	m.sandboxes.Delete(rec.ID)
	m.releaseCapacity(rec)
	if opts.killReason != "" && opts.publish && (opts.entry == nil || !opts.entry.exitNotified) {
		m.publishLifecycleEvent(webhook.EventSandboxKilled, rec, opts.killReason)
	}
	return nil
}

// finalizeDeath applies the crash rule to a runtime that exited on its own.
// A native record becomes UNKNOWN and publishes orphaned once the state is
// on disk; an E2B record stays READY — externally the sandbox keeps running
// until its deadline or an explicit delete — keeping its quota and holding
// only the cleared runtime view. Nothing retries either periodically.
func (m *Manager) finalizeDeath(ctx context.Context, sandboxID string, entry *sandboxEntry, cleanupErr error) {
	entry.state = StateUnknown
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil {
		// The on-disk record keeps its own deadline or startup recovery to
		// finish the transition; the quota stays with the still-READY record.
		ulog.GetLogger().Error("read exited sandbox", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
		return
	}
	if !rec.E2B {
		m.finalizeNativeDeath(ctx, rec, cleanupErr)
		return
	}
	// The released runtime view must not linger on the READY record: the IP
	// went back to the pool and the snapshot refs belonged to the dead run.
	rec.IP, rec.VMMPID = "", 0
	rec.RuntimeSnapshots = nil
	if cleanupErr != nil {
		rec.LastError = cleanupErr.Error()
	}
	if _, err := m.store.Update(ctx, rec); err != nil {
		ulog.GetLogger().Error("persist crashed E2B sandbox", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
		return
	}
	m.sandboxes.Delete(sandboxID)
}

// finalizeNativeDeath persists a crashed native sandbox as UNKNOWN and
// publishes one orphaned event after the write: the ID and the cleanup
// outcome stay inspectable until the client deletes it or startup recovery
// collects it.
func (m *Manager) finalizeNativeDeath(ctx context.Context, rec Record, cleanupErr error) {
	rec.State = StateUnknown
	if cleanupErr == nil {
		rec.LastError = ""
		rec.RuntimeSnapshots = nil
	} else {
		rec.LastError = cleanupErr.Error()
	}
	if _, err := m.store.Update(ctx, rec); err != nil {
		ulog.GetLogger().Error("persist exited native sandbox", ulog.F("sandbox_id", rec.ID), ulog.F("error", err))
		return
	}
	m.publishLifecycleEvent(webhook.EventSandboxKilled, rec, "orphaned")
}

// recoverableLineage reports whether a released runtime leaves a resumable
// record behind: an E2B sandbox whose checkpoint head moved past its source.
func (m *Manager) recoverableLineage(rec Record) bool {
	return rec.E2B && strings.TrimSpace(rec.CheckpointHeadTemplateID) != "" &&
		rec.CheckpointHeadTemplateID != rec.SourceTemplateID
}

// writeBackPaused persists the terminal PAUSED state of a released runtime.
// The checkpoint head is untouched; StartedAt rewinds to the run that
// produced it. The runtime snapshot refs are dropped: they were the released
// run's active/view keys, not checkpoint content, and the resume path
// rebuilds them instead of reading the old ones.
func (m *Manager) writeBackPaused(ctx context.Context, rec Record) error {
	rec.State = StatePaused
	rec.IP, rec.VMMPID = "", 0
	rec.LastError = ""
	rec.ExpiresAt = 0
	rec.RuntimeSnapshots = nil
	if rec.HeadStartedAt > 0 {
		rec.StartedAt = rec.HeadStartedAt
	}
	_, err := m.store.Update(ctx, rec)
	return err
}

// releaseCapacity drops the capacity reservation of a finalized E2B record.
func (m *Manager) releaseCapacity(rec Record) {
	if m.Capacity != nil && rec.E2B {
		m.Capacity.Release(rec.ID)
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

// cleanupSandbox runs under the lifecycle lock. ID-based cleanup is idempotent;
// failures stop before releasing dependent resources. The route slot goes
// first: in-flight proxy requests must stop before the IP returns to the pool.
func (m *Manager) cleanupSandbox(ctx context.Context, sandboxID string, entry *sandboxEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.Routes != nil {
		m.Routes.Release(sandboxID)
	}
	if entry.sbx == nil || entry.sbx.process == nil {
		m.releaseRAM(entry)
	}
	if sbx := entry.sbx; sbx != nil {
		if process := sbx.process; process != nil {
			stopErr := sbx.Stop(ctx)
			if process.Pid() == 0 {
				m.releaseRAM(entry)
			} else {
				// A secondary shutdown error can accompany a confirmed process exit.
				select {
				case <-process.Done():
					m.releaseRAM(entry)
				default:
				}
			}
			if stopErr != nil {
				return stopErr
			}

			for _, path := range []string{process.VmmSocketPath, process.VsockSocketPath} {
				if err := os.RemoveAll(path); err != nil {
					return fmt.Errorf("remove sandbox socket: %w", err)
				}
			}
			sbx.process = nil
		}
		if sbx.slot != nil {
			if err := m.pool.Release(ctx, sbx.slot); err != nil {
				return fmt.Errorf("release network: %w", err)
			}
			sbx.slot = nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.volumeManager.CleanupSandbox(sandboxID, entry.volumes); err != nil {
		return fmt.Errorf("clean volumes: %w", err)
	}
	if err := m.boot.Release(ctx, ReleaseBootRequest{SandboxID: sandboxID}); err != nil {
		return err
	}
	return m.ReleaseCID(sandboxID)
}
