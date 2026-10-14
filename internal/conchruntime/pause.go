package conchruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openeuler/Conch/internal/sandbox"
)

// PauseSandbox captures the sandbox's full memory state as a checkpoint,
// releases its runtime resources and keeps a PAUSED record with no expiry.
// The checkpoint head in that record owns the recoverable state and is used
// to boot the sandbox on the next connect.
//
// expiredBefore revalidates the expiry deadline under the lifecycle lock: a
// zero value skips the check (manual pause), otherwise the sandbox must still
// hold a deadline that was due at that instant, so a TTL extension racing
// with the expiry worker cannot pause a sandbox the client just refreshed.
func (s *Service) PauseSandbox(ctx context.Context, sandboxID string, expiredBefore time.Time) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := s.getSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if !rec.E2B {
		return sandbox.ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not an E2B sandbox", sandboxID))
	}
	if rec.State == sandbox.StatePaused {
		return sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is already paused", sandboxID))
	}
	if rec.State != sandbox.StateReady {
		return sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
	if !expiredBefore.IsZero() && (rec.ExpiresAt <= 0 || rec.ExpiresAt > expiredBefore.UnixNano()) {
		return sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is no longer expired", sandboxID))
	}

	// Capture the full memory state as a checkpoint. On failure the
	// sandbox keeps running under its READY record.
	checkpointed, err := s.Sandbox.Checkpoint(ctx, sandboxID, func(context.Context, sandbox.CheckpointResult) error { return nil })
	if err != nil {
		// The capture machinery leaves the sandbox SUSPENDED when it could
		// not resume the guest after writing the checkpoint; undo that so
		// the sandbox keeps running under its READY record.
		if errors.Is(err, sandbox.ErrCheckpointResume) {
			resumeCtx, resumeCancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeCleanupTimeout)
			defer resumeCancel()
			if current, getErr := s.getSandbox(resumeCtx, sandboxID); getErr == nil && current.State == sandbox.StateSuspended {
				if resumeErr := s.Sandbox.Resume(resumeCtx, sandboxID); resumeErr != nil {
					err = combineOperationErrors(err, resumeErr)
				}
			}
		}
		return err
	}

	// Commit the PAUSED record before releasing the runtime. The write runs
	// under the live request context, so a failure here leaves the sandbox
	// running under its READY record (and its TTL) instead of losing the
	// pause to an unconfirmed teardown.
	paused := rec
	paused.State = sandbox.StatePaused
	paused.ExpiresAt = 0
	paused.RuntimeID = ""
	paused.VMMPID = 0
	paused.IP = ""
	paused.LastError = ""
	paused.CheckpointHeadTemplateID = checkpointed.BootIndexDigest
	if _, err := s.Store.Update(ctx, paused); err != nil {
		return fmt.Errorf("persist paused sandbox state: %w", err)
	}
	// Release the runtime with a detached context: a canceled or timed-out
	// pause must not strand a torn-down runtime. The PAUSED record survives
	// the release; on failure Manager retains it as UNKNOWN with the error,
	// still owning the reservation and the published route.
	releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeCleanupTimeout)
	defer releaseCancel()
	if err := s.Sandbox.ReleaseRuntime(releaseCtx, sandboxID); err != nil {
		return err
	}
	// A guest exit racing the release overwrote the PAUSED record with
	// UNKNOWN: Manager's exit path persists its outcome without checking
	// whether the record was already paused, and the exit callback cannot
	// match the RuntimeID the pause cleared. ReleaseRuntime removed the
	// runtime entry under the Manager lock, so no exit write can land after
	// it: rewrite the record to the pause outcome this call reports.
	if current, getErr := s.getSandbox(releaseCtx, sandboxID); getErr == nil && current.State == sandbox.StateUnknown {
		current.State = sandbox.StatePaused
		current.ExpiresAt = 0
		current.RuntimeID = ""
		current.VMMPID = 0
		current.IP = ""
		current.LastError = ""
		current.CheckpointHeadTemplateID = checkpointed.BootIndexDigest
		if _, err := s.Store.Update(releaseCtx, current); err != nil {
			return fmt.Errorf("restore paused sandbox state: %w", err)
		}
	}
	// The runtime is gone now; only then revoke the published route. If the
	// release failed, the retained record is reconciled instead.
	s.removeProxyRoute(sandboxID)
	s.Capacity.release(sandboxID)
	return nil
}

// ResumePausedSandbox restores a paused sandbox under its original public ID
// with the configuration captured at pause time. CreateSandbox rejects an
// existing ID, so the paused record must leave the store first; if creation
// fails the paused record is restored for a retried connect.
func (s *Service) ResumePausedSandbox(ctx context.Context, sandboxID string, timeout time.Duration) (SandboxCreateResult, error) {
	if s == nil || s.Sandbox == nil {
		return SandboxCreateResult{}, fmt.Errorf("sandbox service is not configured")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	paused, err := s.getSandbox(ctx, sandboxID)
	if err != nil {
		unlock()
		return SandboxCreateResult{}, err
	}
	if !paused.E2B {
		unlock()
		return SandboxCreateResult{}, sandbox.ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not an E2B sandbox", sandboxID))
	}
	if paused.State != sandbox.StatePaused {
		unlock()
		return SandboxCreateResult{}, sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, paused.State))
	}
	templateID := strings.TrimSpace(paused.CheckpointHeadTemplateID)
	if templateID == "" {
		unlock()
		return SandboxCreateResult{}, sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("paused sandbox %s has no resume template", sandboxID))
	}
	// Size the restored sandbox from the resume template's captured CPU count
	// and memory size, matching the named-resume-template create path. The
	// burst ceiling is not part of the record; reproduce a fresh create's
	// derivation — the configured default, raised to the captured CPU count —
	// instead of pinning it to the captured count.
	resume, err := s.inspectResumeTemplate(ctx, templateID)
	if err != nil {
		unlock()
		return SandboxCreateResult{}, err
	}
	vcpuNum, ramMB := paused.VCPUNum, paused.RamMB
	if resume.CPUCount > 0 {
		vcpuNum = resume.CPUCount
	}
	if resume.MemorySizeMB > 0 {
		ramMB = resume.MemorySizeMB
	}
	// A crash between Delete and Create loses the PAUSED record and its sole
	// GC reference to the checkpoint content.
	if err := s.Store.Delete(ctx, sandboxID); err != nil {
		unlock()
		return SandboxCreateResult{}, fmt.Errorf("remove paused sandbox state: %w", err)
	}
	// CreateSandbox takes the same lifecycle lock, so it must run unlocked.
	// PAUSED records own no runtime resources, so nothing else can touch this
	// ID in between; only a concurrent kill could race, and it would fail on
	// the missing record.
	unlock()
	result, err := s.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID:       sandboxID,
		TemplateID:      templateID,
		Metadata:        copyMap(paused.Metadata),
		Network:         paused.Network,
		E2B:             true,
		TimeoutAction:   paused.TimeoutAction,
		AutoResume:      paused.AutoResume,
		MaskRequestHost: paused.MaskRequestHost,
		VCPUNum:         vcpuNum,
		VCPUMax:         max(s.SandboxDefaults.VCPUMax, vcpuNum),
		RamMB:           ramMB,
		Timeout:         timeout,
	})
	if err != nil {
		// CreateSandbox only leaves a record behind when its partial teardown
		// could not be confirmed; otherwise the ID is free again and the
		// paused record can be restored for a retried connect. Probe with a
		// detached context: a canceled request context must not misclassify a
		// retained record as absent.
		probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeCleanupTimeout)
		defer probeCancel()
		if _, getErr := s.getSandbox(probeCtx, sandboxID); getErr == nil {
			return SandboxCreateResult{}, err
		}
		if _, restoreErr := s.Store.Create(context.WithoutCancel(ctx), paused); restoreErr != nil {
			return SandboxCreateResult{}, combineOperationErrors(err, restoreErr)
		}
		return SandboxCreateResult{}, err
	}
	return result, nil
}

// EnsureSandboxRunning makes an E2B sandbox serve data-plane traffic. A
// running sandbox is a no-op that never touches its TTL; a paused sandbox is
// restored from its checkpoint only when it was created with autoResume,
// granted the given TTL. This backs wake-on-traffic: the proxy calls it on a
// route miss before giving up on the sandbox.
func (s *Service) EnsureSandboxRunning(ctx context.Context, sandboxID string, ttl time.Duration) error {
	if s == nil || s.Store == nil {
		return fmt.Errorf("sandbox state store is not configured")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	rec, err := s.getSandbox(ctx, sandboxID)
	if err != nil {
		unlock()
		return err
	}
	if !rec.E2B {
		unlock()
		return sandbox.ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not an E2B sandbox", sandboxID))
	}
	switch rec.State {
	case sandbox.StateReady:
		unlock()
		return nil
	case sandbox.StatePaused:
		if !rec.AutoResume {
			unlock()
			return sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is paused without automatic resume", sandboxID))
		}
	default:
		unlock()
		return sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
	// ResumePausedSandbox takes the same lifecycle lock, so it must run
	// unlocked, matching the connect path.
	unlock()
	if _, err := s.ResumePausedSandbox(ctx, sandboxID, ttl); err != nil {
		// A concurrent waker may have restored the sandbox between our unlock
		// and its own lock: a failure whose record is READY now is a success.
		// Probe with a detached context so a canceled request context cannot
		// misclassify a restored sandbox.
		probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeCleanupTimeout)
		defer probeCancel()
		if current, getErr := s.getSandbox(probeCtx, sandboxID); getErr == nil && current.State == sandbox.StateReady {
			return nil
		}
		return err
	}
	return nil
}

// ApplySandboxTTL updates the E2B expiry deadline of a running sandbox under
// its lifecycle lock. extendOnly keeps a later deadline when one is already
// set (SDK refreshes); otherwise the deadline is reset to now+ttl (connect and
// set_timeout, which may shorten it).
func (s *Service) ApplySandboxTTL(ctx context.Context, sandboxID string, ttl time.Duration, extendOnly bool) (sandbox.Record, error) {
	if s == nil || s.Store == nil {
		return sandbox.Record{}, fmt.Errorf("sandbox state store is not configured")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return sandbox.Record{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, err := s.getSandbox(ctx, sandboxID)
	if err != nil {
		return sandbox.Record{}, err
	}
	if !rec.E2B {
		return sandbox.Record{}, sandbox.ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not an E2B sandbox", sandboxID))
	}
	if rec.State != sandbox.StateReady {
		return sandbox.Record{}, sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, rec.State))
	}
	deadline := time.Now().Add(ttl).UnixNano()
	if extendOnly && rec.ExpiresAt > deadline {
		return rec, nil
	}
	rec.ExpiresAt = deadline
	return s.Store.Update(ctx, rec)
}
