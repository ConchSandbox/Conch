package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/images"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/pkg/ulog"
)

// ResumePausedOptions sizes the resumed run. VMMName is the requested backend;
// the checkpoint boot index decides the actual VMM, and CPUCount plus
// MemorySizeMB in the head metadata decide the sizing.
type ResumePausedOptions struct {
	Timeout time.Duration
	VMMName string
	VCPUMax int64
}

// TTLRequest applies a new expiry to a READY sandbox. ExtendOnly never lowers
// the deadline; MaxLifetime caps the total run lifetime measured from
// StartedAt and rejects runs that already exceeded it.
type TTLRequest struct {
	TTL         time.Duration
	ExtendOnly  bool
	MaxLifetime time.Duration
}

// Pause checkpoints a READY E2B sandbox and releases all of its runtime
// resources under one lock hold. The VMM is stopped before the PAUSED write,
// so the record never carries a PID for a dead runtime; a stop failure hands
// the still-suspended VM back running behind the untouched READY record. The
// record survives as PAUSED with its checkpoint head pinned; an unconfirmed
// release keeps it as UNKNOWN for an explicit delete or startup recovery.
func (m *Manager) Pause(parent context.Context, sandboxID string, expiredBefore time.Time) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	unlock := m.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil {
		return err
	}
	if !rec.E2B {
		return ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not an E2B sandbox", sandboxID))
	}
	if rec.State == StatePaused {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is already paused", sandboxID))
	}
	if rec.State != StateReady {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
	if !expiredBefore.IsZero() && (rec.ExpiresAt <= 0 || rec.ExpiresAt > expiredBefore.UnixNano()) {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is no longer expired", sandboxID))
	}
	entry, err := m.loadSandboxEntry(sandboxID)
	if err != nil {
		// A READY record without a runtime entry is a dead runtime: the expiry
		// dispatcher finishes such records through the delete-only terminal.
		if errors.Is(err, ErrNotFound) {
			return ErrRuntimeDead.Wrap(fmt.Errorf("sandbox %s has no runtime", sandboxID))
		}
		return err
	}
	// A runtime degraded past Ready/Suspended cannot be checkpointed: the
	// expiry dispatcher finishes such records through the crash terminal rule.
	if entry.state != StateReady && entry.state != StateSuspended {
		return ErrRuntimeDead.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, entry.state))
	}
	// The capture hands the VM over suspended: the Stop phase below stops it
	// without a resume round-trip in between. The route withdraws first, so
	// data-plane traffic waits the frozen capture out instead of meeting it.
	m.withdrawRoute(sandboxID)
	txn, err := m.beginCheckpoint(ctx, sandboxID, entry, rec, true)
	if err != nil {
		if errors.Is(err, ErrCheckpointResume) {
			return errors.Join(err, m.undoSuspendedCheckpoint(ctx, sandboxID, entry, rec))
		}
		if errors.Is(err, ErrCheckpointCaptured) {
			return errors.Join(err, m.resumeCapturedSandbox(ctx, sandboxID, entry, rec))
		}
		m.restoreRoute(rec)
		return err
	}
	defer txn.release()
	defer os.RemoveAll(txn.captured.MemRootPath)

	// The record pins only the head digest; the gc.ref.content labels written
	// here chain it to the manifests, config and layers so a released lease
	// never leaves the checkpoint content unreachable.
	labelChildren := images.SetChildrenLabels(m.client.ContentStore(), images.ChildrenHandler(m.client.ContentStore()))
	if err := images.WalkNotEmpty(containerdclient.NewNamespaceContext(ctx), labelChildren, txn.result.Target); err != nil {
		return errors.Join(fmt.Errorf("label checkpoint content: %w", err), m.resumeCapturedSandbox(ctx, sandboxID, entry, rec))
	}

	// The VMM dies before the terminal write. ready preserves the running
	// view for the failures below: after the runtime stopped, the record
	// can no longer be resumed back into it.
	ready := rec
	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
	defer stopCancel()
	entry.state = StateUnknown
	if err := entry.sbx.Stop(stopCtx); err != nil {
		// The VM never stopped and the record never left READY: hand it back
		// running.
		return errors.Join(err, m.resumeCapturedSandbox(stopCtx, sandboxID, entry, ready))
	}
	for _, path := range []string{entry.sbx.process.VmmSocketPath, entry.sbx.process.VsockSocketPath} {
		if err := os.RemoveAll(path); err != nil {
			// The stopped runtime keeps its PID and entry: a delete retries the
			// release through them.
			return m.persistSandboxFailure(stopCtx, ready, true, fmt.Errorf("remove sandbox socket: %w", err))
		}
	}
	entry.sbx.process = nil

	// The head move and the PAUSED terminal state share one record write:
	// after it succeeds the runtime is no longer recoverable. The runtime
	// snapshot refs go with it: they were this run's active/view keys, not
	// checkpoint content — the paused record pins only the head boot index.
	rec.State = StatePaused
	rec.LastError = ""
	rec.IP, rec.VMMPID = "", 0
	rec.ExpiresAt = 0
	rec.RuntimeSnapshots = nil
	rec.CheckpointHeadTemplateID = txn.result.BootIndexDigest
	rec.HeadStartedAt = rec.StartedAt
	rec.HeadCreatedAt = time.Now().UnixNano()
	if _, err := m.store.Update(stopCtx, rec); err != nil {
		// The runtime is already stopped: no resume. The PID and the entry
		// stay for the delete retry.
		return m.persistSandboxFailure(stopCtx, ready, true, err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
	defer cleanupCancel()
	// The overwritten parent head is unreachable from the record now; retire its mem component record while its digest is still known.
	m.retireMemRecord(cleanupCtx, txn.result.ParentBootIndexDigest, rec.SourceTemplateID)
	if err := m.cleanupSandbox(cleanupCtx, sandboxID, entry); err != nil {
		return m.persistSandboxFailure(cleanupCtx, rec, true, err)
	}
	if err := m.releaseCreateLease(cleanupCtx, sandboxID); err != nil {
		return m.persistSandboxFailure(cleanupCtx, rec, true, err)
	}
	m.sandboxes.Delete(sandboxID)
	m.releaseCapacity(rec)
	return nil
}

// undoSuspendedCheckpoint restores a sandbox whose capture failed with
// ErrCheckpointResume: captureCheckpoint already persisted SUSPENDED, so the
// VMM resume either returns both sides to READY or degrades both to UNKNOWN.
func (m *Manager) undoSuspendedCheckpoint(parent context.Context, sandboxID string, entry *sandboxEntry, rec Record) error {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), createCleanupTimeout)
	defer cancel()
	resumeErr := entry.sbx.Resume(saveCtx)
	if resumeErr == nil {
		entry.state = StateReady
		rec.State, rec.LastError = StateReady, ""
		m.restoreRoute(rec)
	} else {
		entry.state = StateUnknown
		rec.State, rec.LastError = StateUnknown, resumeErr.Error()
	}
	_, saveErr := m.store.Update(saveCtx, rec)
	return combineOperationErrors(resumeErr, saveErr)
}

// resumeCapturedSandbox restores a sandbox whose capture succeeded but whose
// pause never committed: the runtime was handed over suspended while the
// record never left READY, so a plain resume is enough. A failed resume
// falls back to undoSuspendedCheckpoint, which retries and persists the
// degraded state.
func (m *Manager) resumeCapturedSandbox(parent context.Context, sandboxID string, entry *sandboxEntry, rec Record) error {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), createCleanupTimeout)
	defer cancel()
	if err := entry.sbx.Resume(saveCtx); err != nil {
		return m.undoSuspendedCheckpoint(parent, sandboxID, entry, rec)
	}
	entry.state = StateReady
	m.restoreRoute(rec)
	return nil
}

// withdrawRoute pulls a sandbox's published route before a checkpoint capture
// freezes the VM: attached requests stop, and the retained slot lets proxy
// waiters block through the capture instead of reading a hard miss.
func (m *Manager) withdrawRoute(sandboxID string) {
	if m.Routes != nil {
		m.Routes.Unpublish(sandboxID)
	}
}

// restoreRoute republishes a runtime that kept running after a failed pause or
// checkpoint; a record without an IP never had a route to restore.
func (m *Manager) restoreRoute(rec Record) {
	if m.Routes == nil || rec.IP == "" {
		return
	}
	if err := m.Routes.Publish(rec.ID, rec.IP, rec.MaskRequestHost); err != nil {
		ulog.GetLogger().Warn("failed to restore sandbox route", ulog.F("sandbox", rec.ID), ulog.F("error", err))
	}
}

// StrandedResume reports whether a non-PAUSED E2B record still carries a
// resumable checkpoint lineage: a resume whose confirmed release lost its
// terminal write (UNKNOWN), or one that died mid-flight (CREATING). Native
// records never qualify: only the E2B terminals strand a record after its
// release was confirmed — a native UNKNOWN keeps the entry and the
// unconfirmed allocation.
func StrandedResume(rec Record) bool {
	if !rec.E2B {
		return false
	}
	head := strings.TrimSpace(rec.CheckpointHeadTemplateID)
	return (rec.State == StateUnknown || rec.State == StateCreating) && head != "" && head != rec.SourceTemplateID
}

// ResumePaused boots a PAUSED sandbox — or a stranded resume, a checkpoint
// lineage left in UNKNOWN or CREATING — from its checkpoint head through the
// create kernel. The whole transition runs under the sandbox's lifecycle
// lock, and a failed resume writes the record back to PAUSED for a retry.
// A record already READY under the lock means a concurrent request finished
// the resume while this one waited: the call succeeds as the joined request
// and the winner's run and deadline stand. A stranded record whose runtime
// entry is still held returns ErrAlreadyExists: another request owns the
// runtime, and the caller waits for its transition instead. The one
// exception is a stranded pause — a PAUSED record whose cleanup failed and
// whose failure write failed with it — whose leftover Unknown entry is
// finished here: an idempotent cleanup retry, the create lease, the entry
// and the capacity reservation all release before the normal resume path
// runs, so a failed retry leaves the strand exactly as it was.
func (m *Manager) ResumePaused(parent context.Context, sandboxID string, opts ResumePausedOptions) (runtimeapi.SandboxCreateResult, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return runtimeapi.SandboxCreateResult{}, ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	if m.Routes == nil || m.Capacity == nil || m.Envd == nil {
		return runtimeapi.SandboxCreateResult{}, fmt.Errorf("E2B runtime dependencies are not configured")
	}
	if opts.Timeout <= 0 {
		return runtimeapi.SandboxCreateResult{}, ErrInvalidArgument.Wrap(fmt.Errorf("timeout must be positive"))
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	unlock := m.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil {
		return runtimeapi.SandboxCreateResult{}, err
	}
	if !rec.E2B {
		return runtimeapi.SandboxCreateResult{}, ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not an E2B sandbox", sandboxID))
	}
	if rec.State == StateReady {
		// A concurrent resume completed while this request waited on the lock.
		return runtimeapi.SandboxCreateResult{}, nil
	}
	if rec.State != StatePaused && !StrandedResume(rec) {
		return runtimeapi.SandboxCreateResult{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
	return m.resumePausedLocked(ctx, rec, opts)
}

// EnsureRunning makes an E2B sandbox serve data-plane traffic. A running
// sandbox is a no-op that never touches its deadline; a paused sandbox — or a
// stranded lineage in UNKNOWN or CREATING — is restored from its checkpoint
// only when it was created with autoResume, granted opts.Timeout as the new
// run's lifetime. This backs wake-on-traffic: concurrent wakers serialize on
// the lifecycle lock, so a loser of the race finds the record READY and
// returns without a second restore.
func (m *Manager) EnsureRunning(parent context.Context, sandboxID string, opts ResumePausedOptions) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	if opts.Timeout <= 0 {
		return ErrInvalidArgument.Wrap(fmt.Errorf("timeout must be positive"))
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	unlock := m.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil {
		return err
	}
	if !rec.E2B {
		return ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not an E2B sandbox", sandboxID))
	}
	switch {
	case rec.State == StateReady:
		return nil
	case (rec.State == StatePaused || StrandedResume(rec)) && rec.AutoResume:
		if m.Routes == nil || m.Capacity == nil || m.Envd == nil {
			return fmt.Errorf("E2B runtime dependencies are not configured")
		}
		_, err := m.resumePausedLocked(ctx, rec, opts)
		return err
	case rec.State == StatePaused || StrandedResume(rec):
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is paused without automatic resume", sandboxID))
	default:
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
}

// resumePausedLocked restores a PAUSED record in place through the create
// kernel. The caller holds the sandbox's lifecycle lock and has validated the
// record's state.
func (m *Manager) resumePausedLocked(ctx context.Context, rec Record, opts ResumePausedOptions) (runtimeapi.SandboxCreateResult, error) {
	head := strings.TrimSpace(rec.CheckpointHeadTemplateID)
	if head == "" {
		return runtimeapi.SandboxCreateResult{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s has no checkpoint head", rec.ID))
	}
	if value, exists := m.sandboxes.Load(rec.ID); exists {
		entry := value.(*sandboxEntry)
		// Any held entry but a stranded pause's belongs to another owner: a
		// CREATING transition in flight or a stranded resume's remains.
		if rec.State != StatePaused || entry.state != StateUnknown {
			return runtimeapi.SandboxCreateResult{}, ErrAlreadyExists.New()
		}
		// The pause's cleanup failed and its failure write failed with it:
		// the PAUSED record kept the quota, the lease and the spent entry.
		// Finish that release — idempotent — before the normal resume path,
		// or the reservation below answers ErrAlreadyExists forever.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
		defer cleanupCancel()
		if err := m.cleanupSandbox(cleanupCtx, rec.ID, entry); err != nil {
			return runtimeapi.SandboxCreateResult{}, err
		}
		if err := m.releaseCreateLease(cleanupCtx, rec.ID); err != nil {
			return runtimeapi.SandboxCreateResult{}, err
		}
		m.sandboxes.Delete(rec.ID)
		m.releaseCapacity(rec)
	}
	req := CreateRequest{
		SandboxID:       rec.ID,
		TemplateID:      head,
		TemplateName:    rec.SourceTemplateName,
		VMMName:         opts.VMMName,
		VCPUNum:         rec.VCPUNum,
		VCPUMax:         max(opts.VCPUMax, rec.VCPUNum),
		RAMMB:           rec.RamMB,
		Network:         rec.Network,
		E2B:             true,
		Timeout:         opts.Timeout,
		TimeoutAction:   rec.TimeoutAction,
		MaskRequestHost: rec.MaskRequestHost,
		Metadata:        cloneStringMap(rec.Metadata),
	}
	if err := m.validateCreateRequest(ctx, &req); err != nil {
		return runtimeapi.SandboxCreateResult{}, err
	}
	// The E2B create prologue: the reservation follows the record, and the
	// route slot is held for the whole bootstrap.
	if err := m.Capacity.Reserve(req.SandboxID, req.VCPUNum, req.RAMMB); err != nil {
		return runtimeapi.SandboxCreateResult{}, err
	}
	routeCtx := m.Routes.Insert(req.SandboxID)
	if routeCtx == nil {
		m.Capacity.Release(req.SandboxID)
		return runtimeapi.SandboxCreateResult{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox proxy registry is closed"))
	}
	var bootstrapCancel context.CancelFunc
	ctx, bootstrapCancel = context.WithTimeout(ctx, bootstrapTimeout)
	defer bootstrapCancel()
	return m.createLocked(ctx, req, routeCtx, &rec)
}

// ApplyTTL applies a TTL change to a READY E2B sandbox and returns the
// updated record. A run whose total lifetime already exceeded MaxLifetime is
// rejected; otherwise the deadline is clamped to the lifetime cap. ExtendOnly
// requests that would not move the deadline forward are no-ops.
func (m *Manager) ApplyTTL(parent context.Context, sandboxID string, req TTLRequest) (Record, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return Record{}, ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	unlock := m.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil {
		return Record{}, err
	}
	if !rec.E2B {
		return Record{}, ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not an E2B sandbox", sandboxID))
	}
	if rec.State != StateReady {
		return Record{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
	if req.MaxLifetime > 0 && time.Since(time.Unix(0, rec.StartedAt)) > req.MaxLifetime {
		return Record{}, ErrInvalidArgument.WrapMessage(fmt.Errorf(
			"sandbox %s ran %s beyond the maximum %s", sandboxID, time.Since(time.Unix(0, rec.StartedAt)).Round(time.Second), req.MaxLifetime,
		), "Max instance length exceeded")
	}
	deadline := time.Now()
	if req.TTL > 0 {
		deadline = deadline.Add(req.TTL)
	}
	if req.MaxLifetime > 0 {
		if capped := time.Unix(0, rec.StartedAt).Add(req.MaxLifetime); deadline.After(capped) {
			deadline = capped
		}
	}
	if req.ExtendOnly && (req.TTL <= 0 || rec.ExpiresAt >= deadline.UnixNano()) {
		return rec, nil
	}
	rec.ExpiresAt = deadline.UnixNano()
	return m.store.Update(ctx, rec)
}

// Expire deletes an expired E2B sandbox. The verdict is deletion whatever the
// timeout action: a live pause-action runtime pauses through Pause, and the
// expiry dispatch reaching here — the kill action or the dead-runtime
// fallback of a failed pause — has nothing left to keep.
func (m *Manager) Expire(parent context.Context, sandboxID string, expiredBefore time.Time) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	unlock := m.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil {
		return err
	}
	if !rec.E2B {
		return ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not an E2B sandbox", sandboxID))
	}
	if rec.State != StateReady {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
	if !expiredBefore.IsZero() && (rec.ExpiresAt <= 0 || rec.ExpiresAt > expiredBefore.UnixNano()) {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is no longer expired", sandboxID))
	}
	var entry *sandboxEntry
	if value, ok := m.sandboxes.Load(sandboxID); ok {
		entry = value.(*sandboxEntry)
		entry.state = StateUnknown
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
	defer cleanupCancel()
	// Retire before cleanup: persistDeleteFailure clears the checkpoint head, so a retry would no longer know it.
	m.retireMemRecord(cleanupCtx, rec.CheckpointHeadTemplateID, rec.SourceTemplateID)
	if entry != nil {
		if err := m.cleanupSandbox(cleanupCtx, sandboxID, entry); err != nil {
			return m.persistDeleteFailure(cleanupCtx, rec, true, err)
		}
	}
	if err := m.releaseCreateLease(cleanupCtx, sandboxID); err != nil {
		return m.persistDeleteFailure(cleanupCtx, rec, true, err)
	}
	return m.finalizeReleased(cleanupCtx, rec, finalizationOptions{
		killReason: "expired", skipRecoverableLineage: true,
	})
}
