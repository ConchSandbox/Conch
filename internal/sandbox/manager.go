package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"syscall"

	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/sandbox/network"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
)

type Manager struct {
	sandboxes     sync.Map
	pool          *network.Pool
	daemonClient  *daemon.Client
}

func NewManager(p *network.Pool, daemonClient *daemon.Client) *Manager {
	return &Manager{
		pool:         p,
		daemonClient: daemonClient,
	}
}

type SandboxCreateRequest struct {
	SnapshotId string `json:"snapshot_id"`
	ImageName  string `json:"image_name"`
	VmmName    string `json:"vmm_name"`
	SandboxId  string `json:"sandbox_id"`
	VcpuNum    int64  `json:"vcpu_num"`
	RamMB      int64  `json:"ram_mb"`
}

type SandboxDeleteRequest struct {
	SandboxId string `json:"sandbox_id"`
}

type SandboxPauseRequest struct {
	SandboxId string `json:"sandbox_id"`
}

func (m *Manager) Create(req SandboxCreateRequest) (string, error) {
	slog.Debug("creating sandbox in manager")

	ctx, cancel := context.WithTimeoutCause(context.Background(), common.RequestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	var sbx *Sandbox
	var peerIP string
	var namespace = common.DefaultNamespace

	parentIDs, err := m.resolveParentSnapshotIDs(context.Background(), namespace, req)
	if err != nil {
		return "", err
	}

	var key = req.SandboxId
	resume := req.SnapshotId != ""

	memOpt := func(info *snapshot.SnapshotConfig) error {
		info.MemSize = req.RamMB
		return nil
	}

	var snapshotConf *snapshot.SnapshotConfig

	if resume {
		slog.Debug("creating sandbox by snapshotId")
		snapshotConf, err = snapshot.AcquireView(context.Background(), namespace, key, parentIDs, memOpt)
	} else {
		slog.Debug("creating sandbox by image", "imageName", req.ImageName)
		snapshotConf, err = snapshot.Prepare(context.Background(), namespace, key, parentIDs, memOpt)
	}

	if err != nil {
		return "", fmt.Errorf("failed to prepare/acquire snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			rmErr := snapshot.Remove(context.Background(), namespace, key)
			if rmErr != nil {
				slog.Error("failed to remove snapshot", "key", key, "err", rmErr)
			} else {
				slog.Info("removed snapshot due to error", "key", key)
			}
		}
	}()

	if resume {
		sbx, err = ResumeSandbox(ctx, snapshotConf, req.VmmName, req.SandboxId, req.VcpuNum, m.pool)
	} else {
		sbx, err = CreateSandbox(ctx, snapshotConf, req.VmmName, req.SandboxId, req.VcpuNum, m.pool)
	}

	if err != nil {
		return "", fmt.Errorf("failed to create sandbox: %w", err)
	}

	peerIP = sbx.slot.VpeerIPString()

	m.sandboxes.Store(req.SandboxId, sbx)
	go func() {
		waitErr := sbx.Wait(ctx)
		if waitErr != nil {
			slog.Warn("failed to wait for sandbox, cleaning up", "err", waitErr)
		}

		cleanupErr := sbx.Close(ctx)
		if cleanupErr != nil {
			slog.Warn("failed to cleanup sandbox, will remove from cache", "err", cleanupErr)
		}

		snapshot.Remove(context.Background(), namespace, req.SandboxId)

		m.sandboxes.Delete(req.SandboxId)
	}()

	slog.Debug("created sandbox in manager")

	return peerIP, nil
}

func (m *Manager) resolveParentSnapshotIDs(
	ctx context.Context,
	namespace string,
	req SandboxCreateRequest,
) (snapshot.ParentSnapshotIDs, error) {
	if req.SnapshotId != "" {
		// snapshot-based startup
		parents, err := snapshot.ResolveParentSnapshotIDs(namespace, req.SnapshotId)
		if err != nil {
			return snapshot.ParentSnapshotIDs{}, err
		}
		return parents, nil
	}
	// image-based startup
	if req.ImageName == "" {
		return snapshot.ParentSnapshotIDs{}, fmt.Errorf("imageName or snapshotID is required")
	}

	rootfsSnapshotID, err := image.GetSnapshotID(ctx, m.daemonClient, namespace, req.ImageName)
	if err != nil {
		return snapshot.ParentSnapshotIDs{}, fmt.Errorf("failed to resolve image snapshot: %w", err)
	}
	parents, err := snapshot.ResolveParentSnapshotIDs(namespace, rootfsSnapshotID)
	if err != nil {
		return snapshot.ParentSnapshotIDs{}, err
	}
	parents.Rootfs = rootfsSnapshotID
	return parents, nil
}

func (m *Manager) Delete(req SandboxDeleteRequest) error {
	ctx, cancel := context.WithTimeoutCause(context.Background(), common.RequestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	sbxVal, exists := m.sandboxes.Load(req.SandboxId)
	if !exists {
		return fmt.Errorf("sandbox %s not found", req.SandboxId)
	}

	sbx, ok := sbxVal.(*Sandbox)
	if !ok {
		return fmt.Errorf("invalid sandbox type for %s", req.SandboxId)
	}

	m.sandboxes.Delete(req.SandboxId)
	go func() {
		err := sbx.Stop(ctx)
		if err != nil {
			slog.Error("sandbox stop error", "sandboxId", req.SandboxId, "err", err)
		}
		var namespace = common.DefaultNamespace
		err = snapshot.Remove(context.Background(), namespace, req.SandboxId)
		if err != nil {
			slog.Error("sandbox remove error", "sandboxId", req.SandboxId, "err", err)
		}
	}()
	return nil
}

func (m *Manager) Pause(req SandboxPauseRequest) (string, error) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), common.RequestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	sbxVal, exists := m.sandboxes.Load(req.SandboxId)
	if !exists {
		return "", fmt.Errorf("sandbox %s not found", req.SandboxId)
	}

	sbx, ok := sbxVal.(*Sandbox)
	if !ok {
		return "", fmt.Errorf("invalid sandbox type for %s", req.SandboxId)
	}

	m.sandboxes.Delete(req.SandboxId)
	defer func() {
		slog.Info("sandbox stop in pause", "sandboxId", req.SandboxId)
		if err := sbx.Stop(ctx); err != nil {
			slog.Error("sandbox stop error after pause", "sandboxId", req.SandboxId, "err", err)
		}
		if err := sbx.Close(ctx); err != nil {
			slog.Error("sandbox close error after pause", "sandboxId", req.SandboxId, "err", err)
		}
		var namespace = common.DefaultNamespace
		if err := snapshot.Remove(context.Background(), namespace, req.SandboxId); err != nil {
			slog.Error("sandbox remove error after pause", "sandboxId", req.SandboxId, "err", err)
		}
	}()

	if err := sbx.Pause(ctx); err != nil {
		return "", fmt.Errorf("sandbox %s pause failed: %w", req.SandboxId, err)
	}

	// TODO: system sync, too large
	syscall.Sync()

	var key = req.SandboxId
	var namespace = common.DefaultNamespace

	info, err := snapshot.Stat(ctx, namespace, key)
	if err != nil {
		return "", fmt.Errorf("failed to stat snapshot %s: %w", key, err)
	}
	parent := info.Parent
	snapshotId, err := snapshot.CalculateSnapshotID(namespace, key, parent)
	if err != nil {
		return "", fmt.Errorf("failed to calculate snapshot id: %w", err)
	}

	err = snapshot.Commit(context.Background(), namespace, snapshotId, key)
	if err != nil {
		return "", fmt.Errorf("error committing snapshot %s: %v", req.SandboxId, err)
	}

	return snapshotId, nil
}

func (m *Manager) CleanupPool() error {
	slog.Debug("cleanup pool begin")
	err := m.pool.Cleanup()
	if err != nil {
		return fmt.Errorf("failed to cleanup pool: %v", err)
	}
	slog.Debug("cleanup pool finish")

	return nil
}
