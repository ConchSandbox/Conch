package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/containerd/v2/core/leases"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	conchimage "github.com/openeuler/Conch/internal/image"
)

// CheckpointResult is the immutable artifact passed to template registration.
type CheckpointResult struct {
	BootIndexDigest       string
	ParentBootIndexDigest string
	Target                ocispec.Descriptor
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
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
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
	parentID := rec.CheckpointHeadTemplateID
	if _, err := conchimage.InspectBootIndex(ctx, m.client, parentID); err != nil {
		return CheckpointResult{}, ErrFailedPrecondition.Wrap(fmt.Errorf(
			"sandbox %s checkpoint head Template %s is unavailable: %w", sandboxID, parentID, err,
		))
	}

	captured, err := m.captureCheckpoint(ctx, sandboxID, entry)
	if err != nil {
		return CheckpointResult{}, err
	}
	defer os.RemoveAll(captured.MemRootPath)

	publishCtx, done, err := m.client.WithLease(containerdclient.NewNamespaceContext(ctx))
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("create checkpoint content lease: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
		defer cancel()
		_ = done(cleanupCtx)
	}()
	leaseID, ok := leases.FromContext(publishCtx)
	if !ok {
		return CheckpointResult{}, fmt.Errorf("checkpoint content lease is missing from context")
	}
	if err := m.client.LeasesService().AddResource(publishCtx, leases.Lease{ID: leaseID}, leases.Resource{
		Type: "content",
		ID:   parentID,
	}); err != nil {
		return CheckpointResult{}, fmt.Errorf("retain checkpoint parent Boot Index %s: %w", parentID, err)
	}
	published, err := conchimage.PublishCheckpointBootIndex(publishCtx, m.client, conchimage.PublishCheckpointBootIndexOptions{
		SourceBootIndexDigest: parentID,
		MemRoot:               captured.MemRootPath,
		VMMName:               captured.VMMName,
		MemorySizeMB:          captured.MemorySizeMB,
	})
	if err != nil {
		return CheckpointResult{}, err
	}
	info, err := conchimage.InspectBootIndexContent(publishCtx, m.client.ContentStore(), published.Target)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("validate published checkpoint boot index: %w", err)
	}
	if !info.Resume {
		return CheckpointResult{}, fmt.Errorf("published checkpoint boot index is not resume-capable")
	}
	if info.BootIndexDigest != published.BootIndexDigest {
		return CheckpointResult{}, fmt.Errorf(
			"validated checkpoint boot index digest %s does not match published digest %s",
			info.BootIndexDigest,
			published.BootIndexDigest,
		)
	}
	if info.VMMName != captured.VMMName {
		return CheckpointResult{}, fmt.Errorf(
			"validated checkpoint VMM %s does not match captured VMM %s",
			info.VMMName,
			captured.VMMName,
		)
	}
	if info.MemorySizeMB != captured.MemorySizeMB {
		return CheckpointResult{}, fmt.Errorf(
			"validated checkpoint memory size %d MB does not match captured size %d MB",
			info.MemorySizeMB,
			captured.MemorySizeMB,
		)
	}
	rec.CheckpointHeadTemplateID = info.BootIndexDigest
	if _, err := m.store.Update(ctx, rec); err != nil {
		return CheckpointResult{}, err
	}
	result := CheckpointResult{BootIndexDigest: info.BootIndexDigest, ParentBootIndexDigest: parentID, Target: published.Target}
	err = register(publishCtx, result)
	if err != nil {
		rec.CheckpointHeadTemplateID = parentID
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
		defer cancel()
		_, rollbackErr := m.store.Update(rollbackCtx, rec)
		return CheckpointResult{}, combineOperationErrors(err, rollbackErr)
	}
	return result, nil
}

func (m *Manager) captureCheckpoint(ctx context.Context, sandboxID string, entry *sandboxEntry) (CapturedBootComponents, error) {
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
