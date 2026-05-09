package sandbox

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/sandbox/network"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/pkg/ulog"
)

type Manager struct {
	sandboxes          sync.Map
	pool               *network.Pool
	daemonClient       *daemon.Client
	vsockSignalRetry   time.Duration
	vsockSignalTimeout time.Duration
	requestTimeout     time.Duration
	cidAllocator       *CIDAllocator
}

func NewManager(p *network.Pool, daemonClient *daemon.Client, vsockSignalRetry, vsockSignalTimeout, requestTimeout time.Duration) *Manager {
	return &Manager{
		pool:               p,
		daemonClient:       daemonClient,
		vsockSignalRetry:   vsockSignalRetry,
		vsockSignalTimeout: vsockSignalTimeout,
		requestTimeout:     requestTimeout,
		cidAllocator:       NewCIDAllocator(),
	}
}

type SandboxCreateRequest struct {
	Namespace   string `json:"namespace"`
	SnapshotId  string `json:"snapshot_id"`
	ImageName   string `json:"image_name"`
	UseSnapshot bool   `json:"use_snapshot"`
	VmmName     string `json:"vmm_name"`
	SandboxId   string `json:"sandbox_id"`
	VcpuNum     int64  `json:"vcpu_num"`
	VcpuMax     int64  `json:"vcpu_max"`
	RamMB       int64  `json:"ram_mb"`
}

type SandboxDeleteRequest struct {
	Namespace string `json:"namespace"`
	SandboxId string `json:"sandbox_id"`
}

type SandboxPauseRequest struct {
	Namespace string `json:"namespace"`
	SandboxId string `json:"sandbox_id"`
}

const (
	vsockReadyPort       = 4065
	expectedAgentVersion = "0.0.2"
)

func sandboxMapKey(namespace, sandboxID string) string {
	return namespace + ":" + sandboxID
}

func createSandboxWithVsockSend(ctx context.Context, snapshotConf *snapshot.SnapshotConfig, namespace, vmmName, sandboxId string, vcpuNum, vcpuMax int64, pool *network.Pool, vsockSignalRetry, vsockSignalTimeout time.Duration, resume bool, vsockCID uint32, vsockSocketPath string) (*Sandbox, error) {
	logger := ulog.GetLogger()

	var sbx *Sandbox
	var createErr error
	if resume {
		sbx, createErr = ResumeSandbox(ctx, snapshotConf, namespace, vmmName, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath)
	} else {
		sbx, createErr = CreateSandbox(ctx, snapshotConf, namespace, vmmName, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath)
	}
	if createErr != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", createErr)
	}

	readyCh := make(chan struct{}, 1)
	go waitForVsockAgentReady(ctx, sbx, sandboxId, vsockSocketPath, vsockSignalRetry, vsockSignalTimeout, readyCh)

	select {
	case <-readyCh:
		logger.Info("Vsock signal sent successfully", ulog.F("sandboxId", sandboxId))
	case <-ctx.Done():
		return sbx, ctx.Err()
	}
	return sbx, nil
}

func waitForVsockAgentReady(ctx context.Context, sbx *Sandbox, sandboxId, vsockSocketPath string, vsockSignalRetry, vsockSignalTimeout time.Duration, readyCh chan struct{}) {
	logger := ulog.GetLogger()
	payload := fmt.Sprintf("I AM SANDBOX_ID:%s\n", sandboxId)

	timer := time.NewTimer(vsockSignalTimeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			logger.Error("vsock signal attempts timed out", ulog.F("sandboxId", sandboxId), ulog.F("timeout", vsockSignalTimeout))
			return
		case <-ctx.Done():
			return
		default:
			conn, err := net.Dial("unix", vsockSocketPath)
			if err != nil {
				logger.Debug("failed to connect to vsock socket, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", err))
				time.Sleep(vsockSignalRetry)
				continue
			}

			_, err = conn.Write([]byte("CONNECT 4065\n"))
			if err != nil {
				logger.Debug("failed to write CONNECT command, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", err))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			_, err = conn.Write([]byte(payload))
			if err != nil {
				logger.Warn("failed to send payload, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", err))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			logger.Debug("payload sent, waiting for OK receipt", ulog.F("sandboxId", sandboxId))
			conn.SetReadDeadline(time.Now().Add(1 * time.Second))

			respBuf := make([]byte, 64)
			n, readErr := conn.Read(respBuf)
			if readErr != nil {
				logger.Debug("failed to read receipt, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", readErr))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			vmmMsg := string(respBuf[:n])
			if !strings.Contains(vmmMsg, "OK") {
				logger.Debug("Unexpected response from VMM proxy", ulog.F("msg", vmmMsg))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			conn.SetReadDeadline(time.Now().Add(2 * time.Second))

			readyBuf := make([]byte, 64)
			rn, rerr := conn.Read(readyBuf)
			if rerr != nil {
				logger.Debug("Waiting for Agent READY signal timed out", ulog.F("error", rerr))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			agentMsg := string(readyBuf[:rn])

			if strings.Contains(agentMsg, "NOT_READY") {
				logger.Error("Agent gRPC service not started", ulog.F("sandboxId", sandboxId))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			if strings.Contains(agentMsg, "READY:") {
				parts := strings.SplitN(agentMsg, "READY:", 2)
				agentVersion := ""
				if len(parts) > 1 {
					agentVersion = strings.TrimSpace(parts[1])
				}

				if agentVersion != expectedAgentVersion {
					logger.Warn("Received agent signal but version mismatch",
						ulog.F("sandboxId", sandboxId),
						ulog.F("agent_version", agentVersion),
						ulog.F("expected_version", expectedAgentVersion))
				} else {
					logger.Info("Sandbox Agent is officially READY!",
						ulog.F("sandboxId", sandboxId),
						ulog.F("agent_version", agentVersion))
				}

				conn.SetReadDeadline(time.Time{})
				sbx.vsockConn = conn
				close(readyCh)
				return
			}
			logger.Warn("Received unknown message from Agent", ulog.F("msg", agentMsg))
			conn.Close()
			time.Sleep(vsockSignalRetry)
		}
	}
}

func (m *Manager) Create(req SandboxCreateRequest) (string, error) {
	logger := ulog.GetLogger()
	logger.Debug("creating sandbox in manager")

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	var sbx *Sandbox
	var peerIP string
	namespace := m.resolveNamespace(req.Namespace)

	parentIDs, err := m.resolveParentSnapshotIDs(context.Background(), namespace, req)
	if err != nil {
		return "", err
	}
	resume := req.SnapshotId != "" || req.UseSnapshot
	if resume && parentIDs.Mem == "" {
		return "", fmt.Errorf("mem snapshot label not found on rootfs snapshot %s", parentIDs.Rootfs)
	}

	var key = req.SandboxId

	memOpt := func(info *snapshot.SnapshotConfig) error {
		info.MemSize = req.RamMB
		return nil
	}

	var snapshotConf *snapshot.SnapshotConfig

	vsockCID, err := m.AllocateUniqueCID(req.SandboxId)
	if err != nil {
		return "", fmt.Errorf("failed to create sandbox: CID allocation error: %v", err)
	}
	vsockSocketPath, err := SandboxVsockSocketPath(key)
	if err != nil {
		return "", fmt.Errorf("failed to create sandbox: vsock socket path error: %v", err)
	}

	vcpuMax := req.VcpuMax
	if vcpuMax == 0 {
		vcpuMax = req.VcpuNum
	}

	if resume {
		logger.Debug("creating sandbox by snapshotId")
		snapshotConf, err = snapshot.AcquireResumeWorkspace(context.Background(), namespace, key, parentIDs, vsockCID, vsockSocketPath, memOpt)
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

	sbx, err = createSandboxWithVsockSend(ctx, snapshotConf, namespace, req.VmmName, req.SandboxId, req.VcpuNum, vcpuMax, m.pool, m.vsockSignalRetry, m.vsockSignalTimeout, resume, vsockCID, vsockSocketPath)

	if err != nil {
		if releaseErr := m.ReleaseCID(req.SandboxId); releaseErr != nil {
			logger.Warn("failed to release CID on create failure", ulog.F("sandbox_id", req.SandboxId), ulog.F("error", releaseErr))
		}
		return "", fmt.Errorf("failed to create sandbox: %w", err)
	}
	peerIP = sbx.slot.VpeerIPString()

	mapKey := sandboxMapKey(namespace, req.SandboxId)
	m.sandboxes.Store(mapKey, sbx)
	go func() {
		waitErr := sbx.Wait(ctx)
		if waitErr != nil {
			logger.Warn("failed to wait for sandbox, cleaning up", ulog.F("error", waitErr))
		}

		cleanupErr := sbx.Close(ctx)
		if cleanupErr != nil {
			logger.Warn("failed to cleanup sandbox, will remove from cache", ulog.F("error", cleanupErr))
		}

		snapshot.Remove(context.Background(), sbx.namespace, req.SandboxId)

		if releaseErr := m.ReleaseCID(req.SandboxId); releaseErr != nil {
			logger.Warn("failed to release CID", ulog.F("sandbox_id", req.SandboxId), ulog.F("error", releaseErr))
		}

		m.sandboxes.Delete(mapKey)
	}()

	logger.Debug("created sandbox in manager")

	return peerIP, nil
}

func (m *Manager) resolveParentSnapshotIDs(
	ctx context.Context,
	namespace string,
	req SandboxCreateRequest,
) (snapshot.ParentSnapshotIDs, error) {
	var rootfsSnapshotID string

	if req.SnapshotId != "" {
		rootfsSnapshotID = req.SnapshotId
	} else {
		if req.ImageName == "" {
			return snapshot.ParentSnapshotIDs{}, fmt.Errorf("imageName or snapshotID is required")
		}

		var err error
		rootfsSnapshotID, err = image.GetSnapshotID(ctx, m.daemonClient, namespace, req.ImageName)
		if err != nil {
			return snapshot.ParentSnapshotIDs{}, fmt.Errorf("failed to resolve image snapshot: %w", err)
		}
	}

	parents, err := snapshot.ResolveImageParentSnapshotIDs(namespace, rootfsSnapshotID)
	if err != nil {
		return snapshot.ParentSnapshotIDs{}, err
	}
	return parents, nil
}

func (m *Manager) resolveNamespace(namespace string) string {
	if namespace != "" {
		return namespace
	}
	return m.daemonClient.DefaultNamespace()
}

func (m *Manager) Delete(req SandboxDeleteRequest) error {
	logger := ulog.GetLogger()

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxId)
	sbxVal, exists := m.sandboxes.Load(mapKey)
	if !exists {
		return fmt.Errorf("sandbox %s not found", req.SandboxId)
	}

	sbx, ok := sbxVal.(*Sandbox)
	if !ok {
		return fmt.Errorf("invalid sandbox type for %s", req.SandboxId)
	}

	m.sandboxes.Delete(mapKey)
	go func() {
		err := sbx.Stop(ctx)
		if err != nil {
			logger.Error("sandbox stop error", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}

		if releaseErr := m.ReleaseCID(req.SandboxId); releaseErr != nil {
			logger.Warn("failed to release CID", ulog.F("sandbox_id", req.SandboxId), ulog.F("error", releaseErr))
		}
	}()
	return nil
}

func (m *Manager) Pause(req SandboxPauseRequest) (string, error) {
	logger := ulog.GetLogger()

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	sbxVal, exists := m.sandboxes.Load(sandboxMapKey(namespace, req.SandboxId))
	if !exists {
		return "", fmt.Errorf("sandbox %s not found", req.SandboxId)
	}

	sbx, ok := sbxVal.(*Sandbox)
	if !ok {
		return "", fmt.Errorf("invalid sandbox type for %s", req.SandboxId)
	}

	m.sandboxes.Delete(sandboxMapKey(sbx.namespace, req.SandboxId))
	defer func() {
		logger.Info("sandbox stop in pause", ulog.F("sandboxId", req.SandboxId))
		if err := sbx.Stop(ctx); err != nil {
			logger.Error("sandbox stop error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
		if err := sbx.Close(ctx); err != nil {
			logger.Error("sandbox close error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
		if err := snapshot.Remove(context.Background(), sbx.namespace, req.SandboxId); err != nil {
			logger.Error("sandbox remove error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
	}()

	if err := sbx.Pause(ctx); err != nil {
		return "", fmt.Errorf("sandbox %s pause failed: %w", req.SandboxId, err)
	}

	// TODO: system sync, too large
	syscall.Sync()

	var key = req.SandboxId

	info, err := snapshot.Stat(ctx, sbx.namespace, key)
	if err != nil {
		return "", fmt.Errorf("failed to stat snapshot %s: %w", key, err)
	}
	parent := info.Parent
	snapshotId, err := snapshot.CalculateSnapshotID(sbx.namespace, key, parent)
	if err != nil {
		return "", fmt.Errorf("failed to calculate snapshot id: %w", err)
	}

	err = snapshot.Commit(context.Background(), sbx.namespace, snapshotId, key)
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

func (m *Manager) AllocateUniqueCID(sandboxId string) (uint32, error) {
	return m.cidAllocator.AllocateCID(sandboxId)
}

func (m *Manager) ReleaseCID(sandboxId string) error {
	return m.cidAllocator.ReleaseCID(sandboxId)
}

func (m *Manager) CleanupCIDMap() error {
	return m.cidAllocator.Cleanup()
}
