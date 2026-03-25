package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/sandbox/network"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/pkg/ulog"
)

type Manager struct {
	sandboxes    sync.Map
	pool         *network.Pool
	daemonClient *daemon.Client
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

const (
	// vsockReadyPort is the vsock port used for agent readiness signaling.
	// Agent connects to (CID=2, vsockReadyPort) after gRPC is ready.
	// cloud-hypervisor forwards this to unix socket: <vsockPath>_<vsockReadyPort>
	vsockReadyPort = 4065
)

func createSandboxWithVsockReady(ctx context.Context, snapshotConf *snapshot.SnapshotConfig, vmmName, sandboxId string, vcpuNum int64, pool *network.Pool) (*Sandbox, error) {
	logger := ulog.GetLogger()

	if err := os.MkdirAll(VsockSocketDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create vsock socket directory: %w", err)
	}

	vsockReadyPath := SandboxVsockSocketPath(sandboxId) + fmt.Sprintf("_%d", vsockReadyPort)
	os.Remove(vsockReadyPath)

	logger.Info("creating vsock ready listener", ulog.F("path", vsockReadyPath), ulog.F("sandboxId", sandboxId))
	readyListener, listenErr := net.Listen("unix", vsockReadyPath)
	if listenErr != nil {
		return nil, fmt.Errorf("failed to create vsock ready listener: %w", listenErr)
	}
	defer readyListener.Close()
	defer os.Remove(vsockReadyPath)

	sbx, err := CreateSandbox(ctx, snapshotConf, vmmName, sandboxId, vcpuNum, pool)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", err)
	}

	readyCh := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := readyListener.Accept()
		if acceptErr != nil {
			logger.Debug("vsock ready listener accept error (expected on cleanup)", ulog.F("error", acceptErr), ulog.F("sandboxId", sandboxId))
			return
		}
		logger.Info("vsock connection accepted from agent", ulog.F("sandboxId", sandboxId))
		conn.Write([]byte("ACK"))
		conn.Close()
		close(readyCh)
	}()

	select {
	case <-readyCh:
		logger.Info("agent ready signal received via vsock", ulog.F("sandboxId", sandboxId))
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout waiting for agent ready: %w", ctx.Err())
	}

	return sbx, nil
}

func (m *Manager) Create(req SandboxCreateRequest) (string, error) {
	logger := ulog.GetLogger()
	logger.Debug("creating sandbox in manager")

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
		logger.Debug("creating sandbox by snapshotId")
		snapshotConf, err = snapshot.AcquireView(context.Background(), namespace, key, parentIDs, memOpt)
	} else {
		logger.Debug("creating sandbox by image", ulog.F("imageName", req.ImageName))
		snapshotConf, err = snapshot.Prepare(context.Background(), namespace, key, parentIDs, memOpt)
	}

	if err != nil {
		return "", fmt.Errorf("failed to prepare/acquire snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			rmErr := snapshot.Remove(context.Background(), namespace, key)
			if rmErr != nil {
				logger.Error("failed to remove snapshot", ulog.F("key", key), ulog.F("error", rmErr))
			} else {
				logger.Info("removed snapshot due to error", ulog.F("key", key))
			}
		}
	}()

	if resume {
		sbx, err = ResumeSandbox(ctx, snapshotConf, req.VmmName, req.SandboxId, req.VcpuNum, m.pool)

	} else {
		sbx, err = createSandboxWithVsockReady(ctx, snapshotConf, req.VmmName, req.SandboxId, req.VcpuNum, m.pool)
	}
	if err != nil {
		return "", fmt.Errorf("failed to create sandbox: %w", err)
	}
	peerIP = sbx.slot.VpeerIPString()

	m.sandboxes.Store(req.SandboxId, sbx)
	go func() {
		waitErr := sbx.Wait(ctx)
		if waitErr != nil {
			logger.Warn("failed to wait for sandbox, cleaning up", ulog.F("error", waitErr))
		}

		cleanupErr := sbx.Close(ctx)
		if cleanupErr != nil {
			logger.Warn("failed to cleanup sandbox, will remove from cache", ulog.F("error", cleanupErr))
		}

		snapshot.Remove(context.Background(), namespace, req.SandboxId)

		m.sandboxes.Delete(req.SandboxId)
	}()

	logger.Debug("created sandbox in manager")

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
	logger := ulog.GetLogger()

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
			logger.Error("sandbox stop error", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
	}()
	return nil
}

func (m *Manager) Pause(req SandboxPauseRequest) (string, error) {
	logger := ulog.GetLogger()

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
		logger.Info("sandbox stop in pause", ulog.F("sandboxId", req.SandboxId))
		if err := sbx.Stop(ctx); err != nil {
			logger.Error("sandbox stop error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
		if err := sbx.Close(ctx); err != nil {
			logger.Error("sandbox close error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
		var namespace = common.DefaultNamespace
		if err := snapshot.Remove(context.Background(), namespace, req.SandboxId); err != nil {
			logger.Error("sandbox remove error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
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
	logger := ulog.GetLogger()
	logger.Debug("cleanup pool begin")
	err := m.pool.Cleanup()
	if err != nil {
		return fmt.Errorf("failed to cleanup pool: %v", err)
	}
	logger.Debug("cleanup pool finish")

	return nil
}
