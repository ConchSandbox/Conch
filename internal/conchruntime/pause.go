package conchruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openeuler/Conch/internal/id"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/pkg/ulog"
)

// PauseSandbox captures the sandbox's full memory state as a resume template,
// releases its runtime resources and keeps a PAUSED record with no expiry.
// The pause template owns the recoverable state; the sandbox boots from its
// checkpoint head on the next connect.
func (s *Service) PauseSandbox(ctx context.Context, sandboxID string) error {
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
	if len(rec.VolumeMounts) > 0 {
		return sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s has volume mounts, pause is not supported", sandboxID))
	}

	// Capture the full memory state as a resume template. On failure the
	// sandbox keeps running under its READY record.
	pauseTemplate, err := id.NewWithPrefix("pause-")
	if err != nil {
		return err
	}
	// Manager.Checkpoint owns capture, head persistence and template
	// registration; the register callback runs under its content lease.
	checkpointed, err := s.Sandbox.Checkpoint(ctx, sandboxID, func(ctx context.Context, result sandbox.CheckpointResult) error {
		if s.Templates == nil {
			return fmt.Errorf("template store is not configured")
		}
		_, err := s.Templates.Put(ctx, conchtemplate.Entry{
			Name: pauseTemplate, Origin: conchtemplate.OriginCheckpoint, BootMode: conchtemplate.BootModeResume,
			BootIndexDigest: result.BootIndexDigest, ParentBootIndexDigest: result.ParentBootIndexDigest,
			SourceSandboxID: sandboxID,
		}, result.Target)
		return err
	})
	if err != nil {
		return err
	}

	// Mirror removeSandboxLocked teardown, but persist a PAUSED record instead
	// of deleting it: the resume template owns the recoverable state.
	s.removeProxyRoute(sandboxID)
	cleanupErr := s.Sandbox.Delete(ctx, sandboxID)
	if errors.Is(cleanupErr, sandbox.ErrNotFound) {
		cleanupErr = nil
	}
	if errors.Is(cleanupErr, sandbox.ErrFailedPrecondition) {
		return cleanupErr
	}
	if cleanupErr != nil {
		// Manager retained the (UNKNOWN) record; it still owns the
		// reservation so the maintenance worker can retry the release.
		return cleanupErr
	}
	s.Capacity.release(sandboxID)
	// Manager.Delete removed the READY record. Re-create it as PAUSED: the
	// paused record restarts from the post-checkpoint head with no runtime
	// identity and owns no reservation.
	paused := rec
	paused.State = sandbox.StatePaused
	paused.ExpiresAt = 0
	paused.RuntimeID = ""
	paused.VMMPID = 0
	paused.IP = ""
	paused.LastError = ""
	paused.CheckpointHeadTemplateID = checkpointed.BootIndexDigest
	paused.PauseTemplateName = pauseTemplate
	if _, err := s.Store.Create(ctx, paused); err != nil {
		return fmt.Errorf("persist paused sandbox state: %w", err)
	}
	return nil
}

// ResumePausedSandbox restores a paused sandbox under its original public ID
// with the configuration captured at pause time. CreateSandbox rejects an
// existing ID, so the paused record must leave the store first; if creation
// fails the paused record is restored for a retried connect.
func (s *Service) ResumePausedSandbox(ctx context.Context, sandboxID string, opts SandboxResumeOptions) (SandboxCreateResult, error) {
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
	// and memory size, matching the named-resume-template create path.
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
	// A crash between Delete and Create loses the record but not the resume
	// template, which stays GC-pinned and can be booted manually.
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
		Env:             copyMap(paused.Env),
		Metadata:        copyMap(paused.Metadata),
		Network:         paused.Network,
		E2B:             true,
		TimeoutAction:   paused.TimeoutAction,
		MaskRequestHost: paused.MaskRequestHost,
		VCPUNum:         vcpuNum,
		VCPUMax:         vcpuNum,
		RamMB:           ramMB,
		Timeout:         opts.Timeout,
	})
	if err != nil {
		// CreateSandbox only leaves a record behind when its partial teardown
		// could not be confirmed; otherwise the ID is free again and the
		// paused record can be restored for a retried connect.
		if _, getErr := s.getSandbox(ctx, sandboxID); getErr == nil {
			return SandboxCreateResult{}, err
		}
		if _, restoreErr := s.Store.Create(context.WithoutCancel(ctx), paused); restoreErr != nil {
			return SandboxCreateResult{}, combineOperationErrors(err, restoreErr)
		}
		return SandboxCreateResult{}, err
	}
	// The pause template is single-use once resumed; the resume template that
	// CreateSandbox pinned is the new checkpoint head.
	if paused.PauseTemplateName != "" && s.Templates != nil {
		if err := s.Templates.Delete(ctx, paused.PauseTemplateName); err != nil && !errors.Is(err, conchtemplate.ErrNotFound) {
			ulog.GetLogger().Warn("failed to remove pause template after resume",
				ulog.F("sandbox_id", sandboxID),
				ulog.F("template", paused.PauseTemplateName),
				ulog.F("error", err),
			)
		}
	}
	return result, nil
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
