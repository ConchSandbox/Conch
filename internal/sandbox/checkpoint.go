package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/vmm/driver"
	"github.com/openeuler/Conch/pkg/ulog"
)

// CheckpointResult is the immutable artifact passed to template registration.
type CheckpointResult struct {
	BootIndexDigest       string
	ParentBootIndexDigest string
	Target                ocispec.Descriptor
}

// checkpointTransaction holds the captured artifacts and the content lease of
// an in-flight checkpoint. beginCheckpoint publishes the boot index; the
// caller commits the record head move and owns MemRoot cleanup and lease
// release.
type checkpointTransaction struct {
	ctx      context.Context
	result   CheckpointResult
	captured CapturedBootComponents
	release  func()
}

// Checkpoint serializes capture, head persistence and template registration.
// register runs under the content lease; Manager does not own a Template Store.
func (m *Manager) Checkpoint(parent context.Context, sandboxID string, register func(context.Context, CheckpointResult) error) (CheckpointResult, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return CheckpointResult{}, ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	if register == nil {
		return CheckpointResult{}, fmt.Errorf("checkpoint registration is required")
	}
	// The capture alone may poll for minutes, past the default request budget.
	ctx, cancel := context.WithTimeout(parent, max(m.requestTimeout, driver.SnapshotCaptureTimeout))
	defer cancel()
	unlock := m.lifecycleLocks.lock(sandboxID)
	defer unlock()
	entry, err := m.loadSandboxEntry(sandboxID)
	if err != nil {
		return CheckpointResult{}, err
	}
	rec, err := m.store.Get(ctx, sandboxID)
	if err != nil {
		return CheckpointResult{}, err
	}
	// The capture freezes the VM: withdraw the route first so data-plane
	// traffic waits the capture out instead of meeting it frozen.
	m.withdrawRoute(sandboxID)
	txn, err := m.beginCheckpoint(ctx, sandboxID, entry, rec, false)
	if err != nil {
		// A resume failure leaves the runtime suspended under the SUSPENDED
		// record captureCheckpoint persisted: undo it — republishing the route
		// on success — instead of stranding the sandbox in SUSPENDED.
		if errors.Is(err, ErrCheckpointResume) {
			return CheckpointResult{}, errors.Join(err, m.undoSuspendedCheckpoint(ctx, sandboxID, entry, rec))
		}
		m.restoreRoute(ctx, rec)
		return CheckpointResult{}, err
	}
	defer txn.release()
	defer os.RemoveAll(txn.captured.MemRootPath)
	// The capture already resumed the runtime: serve traffic again while the
	// head move and the registration proceed.
	m.restoreRoute(ctx, rec)

	previousHeadStartedAt, previousHeadCreatedAt := rec.HeadStartedAt, rec.HeadCreatedAt
	rec.CheckpointHeadTemplateID = txn.result.BootIndexDigest
	rec.HeadStartedAt = rec.StartedAt
	rec.HeadCreatedAt = time.Now().UnixNano()
	if _, err := m.store.Update(ctx, rec); err != nil {
		return CheckpointResult{}, err
	}
	result := txn.result
	err = register(txn.ctx, result)
	if err != nil {
		rec.CheckpointHeadTemplateID = txn.result.ParentBootIndexDigest
		rec.HeadStartedAt, rec.HeadCreatedAt = previousHeadStartedAt, previousHeadCreatedAt
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
		defer cancel()
		_, rollbackErr := m.store.Update(rollbackCtx, rec)
		return CheckpointResult{}, combineOperationErrors(err, rollbackErr)
	}
	retireCtx, cancelRetire := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
	defer cancelRetire()
	// The parent head is superseded; the rollback path above restores it as current, so only this path retires.
	m.retireMemRecord(retireCtx, txn.result.ParentBootIndexDigest, rec.SourceTemplateID)
	return result, nil
}

// ErrCheckpointCaptured marks a checkpoint that failed after its capture
// already succeeded with LeavePaused: the runtime stays suspended while the
// record was never modified, so the caller must resume the runtime before
// returning the failure.
var ErrCheckpointCaptured = errors.New("checkpoint failed after capture")

// beginCheckpoint captures and publishes a checkpoint boot index under a
// content lease without touching the record. leavePaused hands a running
// runtime to the caller still suspended after a successful capture. On error
// the captured artifacts and the lease are already cleaned up; with
// leavePaused, failures after the capture are wrapped with
// ErrCheckpointCaptured so the caller resumes the suspended runtime.
func (m *Manager) beginCheckpoint(ctx context.Context, sandboxID string, entry *sandboxEntry, rec Record, leavePaused bool) (*checkpointTransaction, error) {
	parentID := rec.CheckpointHeadTemplateID
	if _, err := conchimage.InspectBootIndex(ctx, m.client, parentID); err != nil {
		return nil, ErrFailedPrecondition.Wrap(fmt.Errorf(
			"sandbox %s checkpoint head Template %s is unavailable: %w", sandboxID, parentID, err,
		))
	}
	captured, err := m.captureCheckpoint(ctx, sandboxID, entry, leavePaused)
	if err != nil {
		return nil, err
	}
	publishCtx, done, err := m.client.WithLease(containerdclient.NewNamespaceContext(ctx))
	if err != nil {
		os.RemoveAll(captured.MemRootPath)
		if leavePaused {
			err = fmt.Errorf("%w: %w", ErrCheckpointCaptured, err)
		}
		return nil, err
	}
	txn := &checkpointTransaction{
		ctx:      publishCtx,
		captured: captured,
		release: func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
			defer cancel()
			if err := done(containerdclient.NewNamespaceContext(cleanupCtx)); err != nil {
				ulog.WithContext(cleanupCtx).Warn("failed to release checkpoint lease", ulog.F("error", err))
			}
		},
	}
	fail := func(err error) (*checkpointTransaction, error) {
		txn.release()
		os.RemoveAll(captured.MemRootPath)
		if leavePaused {
			err = fmt.Errorf("%w: %w", ErrCheckpointCaptured, err)
		}
		return nil, err
	}
	leaseID, ok := leases.FromContext(publishCtx)
	if !ok {
		return fail(fmt.Errorf("checkpoint content lease is missing from context"))
	}
	if err := m.client.LeasesService().AddResource(publishCtx, leases.Lease{ID: leaseID}, leases.Resource{
		Type: "content",
		ID:   parentID,
	}); err != nil {
		return fail(fmt.Errorf("retain checkpoint parent Boot Index %s: %w", parentID, err))
	}
	published, err := conchimage.PublishCheckpointBootIndex(publishCtx, m.client, conchimage.PublishCheckpointBootIndexOptions{
		SourceBootIndexDigest: parentID,
		MemRoot:               captured.MemRootPath,
		VMMName:               captured.VMMName,
		MemorySizeMB:          captured.MemorySizeMB,
		CPUCount:              rec.VCPUNum,
	})
	if err != nil {
		return fail(err)
	}
	info, err := conchimage.InspectBootIndexContent(publishCtx, m.client.ContentStore(), published.Target)
	if err != nil {
		return fail(fmt.Errorf("validate published checkpoint boot index: %w", err))
	}
	if !info.Resume {
		return fail(fmt.Errorf("published checkpoint boot index is not resume-capable"))
	}
	if info.BootIndexDigest != published.BootIndexDigest {
		return fail(fmt.Errorf(
			"validated checkpoint boot index digest %s does not match published digest %s",
			info.BootIndexDigest,
			published.BootIndexDigest,
		))
	}
	if info.VMMName != captured.VMMName {
		return fail(fmt.Errorf(
			"validated checkpoint VMM %s does not match captured VMM %s",
			info.VMMName,
			captured.VMMName,
		))
	}
	if info.MemorySizeMB != captured.MemorySizeMB {
		return fail(fmt.Errorf(
			"validated checkpoint memory size %d MB does not match captured size %d MB",
			info.MemorySizeMB,
			captured.MemorySizeMB,
		))
	}
	txn.result = CheckpointResult{BootIndexDigest: info.BootIndexDigest, ParentBootIndexDigest: parentID, Target: published.Target}
	return txn, nil
}

func (m *Manager) captureCheckpoint(ctx context.Context, sandboxID string, entry *sandboxEntry, leavePaused bool) (CapturedBootComponents, error) {
	wasSuspended := entry.state == StateSuspended
	if entry.state != StateReady && !wasSuspended {
		return CapturedBootComponents{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", sandboxID, entry.state))
	}
	sbx := entry.sbx
	if sbx == nil {
		return CapturedBootComponents{}, fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", sandboxID)
	}
	if len(sbx.vmStartSpec.VirtioFS) > 0 {
		return CapturedBootComponents{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s has volume mounts, checkpoint is not supported", sandboxID))
	}
	captured, err := m.checkpointCapture.Capture(ctx, RuntimeCaptureRequest{
		Source:      sbx,
		PauseBefore: !wasSuspended,
		LeavePaused: leavePaused,
	})
	if err != nil {
		if errors.Is(err, ErrCheckpointResume) {
			entry.state = StateSuspended
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
			defer cancel()
			rec, getErr := m.store.Get(saveCtx, sandboxID)
			if getErr == nil {
				rec.State, rec.LastError = StateSuspended, err.Error()
				_, getErr = m.store.Update(saveCtx, rec)
			}
			err = combineOperationErrors(err, getErr)
		}
		return CapturedBootComponents{}, fmt.Errorf("sandbox %s checkpoint failed: %w", sandboxID, err)
	}

	return captured, nil
}
