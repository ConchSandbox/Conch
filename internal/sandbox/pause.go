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
// resources under one lock hold. The record survives as PAUSED with its
// checkpoint head pinned; an unconfirmed cleanup keeps it as UNKNOWN for an
// explicit delete or startup recovery.
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
	// The capture hands the VM over suspended: the cleanup below stops it
	// without a resume round-trip in between.
	txn, err := m.beginCheckpoint(ctx, sandboxID, entry, rec, true)
	if err != nil {
		if errors.Is(err, ErrCheckpointResume) {
			return errors.Join(err, m.undoSuspendedCheckpoint(ctx, sandboxID, entry, rec))
		}
		if errors.Is(err, ErrCheckpointCaptured) {
			return errors.Join(err, m.resumeCapturedSandbox(ctx, sandboxID, entry, rec))
		}
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

	// The head move and the PAUSED terminal state share one record write:
	// after it succeeds the runtime is no longer recoverable as READY. The
	// runtime snapshot refs go with it: they are this run's active/view keys,
	// released by ID in the cleanup below, not checkpoint content — the
	// paused record pins only the head boot index.
	// ready preserves the running view in case the PAUSED write fails.
	ready := rec
	rec.State = StatePaused
	rec.LastError = ""
	rec.IP, rec.VMMPID = "", 0
	rec.ExpiresAt = 0
	rec.RuntimeSnapshots = nil
	rec.CheckpointHeadTemplateID = txn.result.BootIndexDigest
	rec.HeadStartedAt = rec.StartedAt
	rec.HeadCreatedAt = time.Now().UnixNano()
	if _, err := m.store.Update(ctx, rec); err != nil {
		return errors.Join(err, m.resumeCapturedSandbox(ctx, sandboxID, entry, ready))
	}
	entry.state = StateUnknown
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
	defer cleanupCancel()
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
	return nil
}

// ResumePaused boots a PAUSED sandbox from its checkpoint head through the
// create kernel. The whole transition runs under the sandbox's lifecycle
// lock, and a failed resume writes the record back to PAUSED for a retry.
// A record already READY under the lock means a concurrent request finished
// the resume while this one waited: the call succeeds as the joined request
// and the winner's run and deadline stand.
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
	if rec.State != StatePaused {
		return runtimeapi.SandboxCreateResult{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
	return m.resumePausedLocked(ctx, rec, opts)
}

// EnsureRunning makes an E2B sandbox serve data-plane traffic. A running
// sandbox is a no-op that never touches its deadline; a paused sandbox is
// restored from its checkpoint only when it was created with autoResume,
// granted opts.Timeout as the new run's lifetime. This backs wake-on-traffic:
// concurrent wakers serialize on the lifecycle lock, so a loser of the race
// finds the record READY and returns without a second restore.
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
	case rec.State == StatePaused && rec.AutoResume:
		if m.Routes == nil || m.Capacity == nil || m.Envd == nil {
			return fmt.Errorf("E2B runtime dependencies are not configured")
		}
		_, err := m.resumePausedLocked(ctx, rec, opts)
		return err
	case rec.State == StatePaused:
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
	if _, exists := m.sandboxes.Load(rec.ID); exists {
		return runtimeapi.SandboxCreateResult{}, ErrAlreadyExists.New()
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
