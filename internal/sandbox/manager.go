package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/agent/hostconn"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/pkg/ulog"
)

type Manager struct {
	sandboxes          sync.Map // map[string]*sandboxEntry
	pool               *netstack.Pool
	daemonClient       *containerdclient.Client
	snapshotServer     *snapshot.Server
	vsockSignalRetry   time.Duration
	vsockSignalTimeout time.Duration
	requestTimeout     time.Duration
	cidAllocator       *CIDAllocator
}

type sandboxLifecycleState uint8

const (
	sandboxCreating sandboxLifecycleState = iota
	sandboxReady
	sandboxPausing
	sandboxDeleting
	sandboxExited
)

func (s sandboxLifecycleState) String() string {
	switch s {
	case sandboxCreating:
		return "creating"
	case sandboxReady:
		return "ready"
	case sandboxPausing:
		return "pausing"
	case sandboxDeleting:
		return "deleting"
	case sandboxExited:
		return "exited"
	default:
		return "unknown"
	}
}

type sandboxEntry struct {
	mu    sync.Mutex
	state sandboxLifecycleState
	sbx   *Sandbox
}

func NewManager(p *netstack.Pool, daemonClient *containerdclient.Client, snapshotServer *snapshot.Server, vsockSignalRetry, vsockSignalTimeout, requestTimeout time.Duration) (*Manager, error) {
	if snapshotServer == nil {
		return nil, fmt.Errorf("snapshot server is required")
	}
	return &Manager{
		pool:               p,
		daemonClient:       daemonClient,
		snapshotServer:     snapshotServer,
		vsockSignalRetry:   vsockSignalRetry,
		vsockSignalTimeout: vsockSignalTimeout,
		requestTimeout:     requestTimeout,
		cidAllocator:       NewCIDAllocator(),
	}, nil
}

type SandboxCreateRequest struct {
	Namespace   string `json:"namespace"`
	SnapshotId  string `json:"snapshot_id"`
	ImageName   string `json:"image_name"`
	UseSnapshot bool   `json:"use_snapshot"`
	VmmName     string `json:"vmm_name"`
	SandboxId   string `json:"sandbox_id"`
	LeaseID     string `json:"lease_id,omitempty"`
	VcpuNum     int64  `json:"vcpu_num"`
	VcpuMax     int64  `json:"vcpu_max"`
	RamMB       int64  `json:"ram_mb"`
	AgentToken  string `json:"-"`
}

type SandboxDeleteRequest struct {
	Namespace string `json:"namespace"`
	SandboxId string `json:"sandbox_id"`
}

type SandboxPauseRequest struct {
	Namespace string `json:"namespace"`
	SandboxId string `json:"sandbox_id"`
}

type SandboxCreateResult struct {
	IP              string
	AgentToken      string
	Namespace       string
	SandboxID       string
	LeaseID         string
	VMMPID          int
	VMMSocketPath   string
	VsockCID        uint32
	VsockSocketPath string
	NetworkSlotKey  string
	NetworkNS       string
	RootfsKey       string
	MemKey          string
	ParentRootfsID  string
	ParentMemID     string
	ParentVMID      string
	RootfsMount     string
	MemMount        string
	VMMount         string
	RootDir         string
	MemSize         int64
	Resume          bool
}

func GenerateAgentToken() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("failed to generate agent token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(tokenBytes), nil
}

func sandboxMapKey(namespace, sandboxID string) string {
	return namespace + ":" + sandboxID
}

func (m *Manager) reserveSandboxEntry(namespace, sandboxID string) (string, *sandboxEntry, error) {
	mapKey := sandboxMapKey(namespace, sandboxID)
	entry := &sandboxEntry{state: sandboxCreating}
	entry.mu.Lock()

	actual, loaded := m.sandboxes.LoadOrStore(mapKey, entry)
	if !loaded {
		return mapKey, entry, nil
	}
	entry.mu.Unlock()

	existing, ok := actual.(*sandboxEntry)
	if !ok {
		return "", nil, fmt.Errorf("invalid sandbox entry type for %s", sandboxID)
	}
	existing.mu.Lock()
	state := existing.state
	existing.mu.Unlock()
	return "", nil, fmt.Errorf("sandbox %s already exists or is %s", sandboxID, state)
}

func (m *Manager) loadSandboxEntry(mapKey, sandboxID string) (*sandboxEntry, error) {
	entryVal, exists := m.sandboxes.Load(mapKey)
	if !exists {
		return nil, fmt.Errorf("sandbox %s not found", sandboxID)
	}
	entry, ok := entryVal.(*sandboxEntry)
	if !ok {
		return nil, fmt.Errorf("invalid sandbox entry type for %s", sandboxID)
	}
	return entry, nil
}

func (m *Manager) isCurrentSandboxEntry(mapKey string, entry *sandboxEntry) bool {
	actual, ok := m.sandboxes.Load(mapKey)
	return ok && actual == entry
}

func createSandboxWithVsockSend(ctx context.Context, vmStartSpec VMStartSpec, namespace, vmmName, sandboxId, agentToken string, vcpuNum, vcpuMax int64, pool *netstack.Pool, vsockSignalRetry, vsockSignalTimeout time.Duration, resume bool, vsockCID uint32, vsockSocketPath string) (*Sandbox, error) {
	logger := ulog.GetLogger()

	var sbx *Sandbox
	var createErr error
	if resume {
		sbx, createErr = ResumeSandbox(ctx, vmStartSpec, namespace, vmmName, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath)
	} else {
		sbx, createErr = CreateSandbox(ctx, vmStartSpec, namespace, vmmName, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath)
	}
	if createErr != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", createErr)
	}

	// WaitReady returns timeout and context cancellation errors directly.
	conn, err := hostconn.WaitReady(ctx, hostconn.ReadyOptions{
		SandboxID:       sandboxId,
		AgentToken:      agentToken,
		VMMName:         vmmName,
		VsockCID:        vsockCID,
		VsockSocketPath: vsockSocketPath,
		Retry:           vsockSignalRetry,
		Timeout:         vsockSignalTimeout,
	})
	if err != nil {
		return sbx, err
	}
	sbx.vsockConn = conn
	logger.Info("Vsock signal sent successfully", ulog.F("sandboxId", sandboxId))
	return sbx, nil
}

type createRuntimeIDs struct {
	key             string
	vsockCID        uint32
	vsockSocketPath string
	vcpuMax         int64
}

func (m *Manager) Create(req SandboxCreateRequest) (result SandboxCreateResult, err error) {
	logger := ulog.GetLogger()
	logger.Debug("creating sandbox in manager")

	if req.AgentToken == "" {
		return SandboxCreateResult{}, fmt.Errorf("agent token is required")
	}

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey, entry, err := m.reserveSandboxEntry(namespace, req.SandboxId)
	if err != nil {
		return SandboxCreateResult{}, err
	}
	defer entry.mu.Unlock()
	defer func() {
		if err != nil {
			m.sandboxes.CompareAndDelete(mapKey, entry)
		}
	}()

	leaseCtx, leaseID, err := m.prepareRuntimeLease(ctx, namespace, req)
	if err != nil {
		return SandboxCreateResult{}, err
	}

	parentIDs, resume, err := m.prepareParentSnapshots(leaseCtx, namespace, req)
	if err != nil {
		return SandboxCreateResult{}, err
	}

	runtimeIDs, err := m.allocateCreateRuntimeIDs(req)
	if err != nil {
		return SandboxCreateResult{}, err
	}
	cidAllocated := true
	defer func() {
		if err != nil && cidAllocated {
			if releaseErr := m.ReleaseCID(req.SandboxId); releaseErr != nil {
				logger.Warn("failed to release CID on create failure", ulog.F("sandbox_id", req.SandboxId), ulog.F("error", releaseErr))
			}
		}
	}()

	layout, err := m.prepareBootLayout(leaseCtx, namespace, req, parentIDs, runtimeIDs, resume)
	if err != nil {
		return SandboxCreateResult{}, err
	}
	defer func() {
		if err == nil {
			return
		}
		rmErr := m.snapshotServer.ReleaseBootLayout(leaseCtx, namespace, runtimeIDs.key)
		if rmErr != nil {
			logger.Error("failed to release snapshot layout", ulog.F("key", runtimeIDs.key), ulog.F("error", rmErr))
			return
		}
		logger.Info("released snapshot layout due to error", ulog.F("key", runtimeIDs.key))
	}()

	vmStartSpec := vmStartSpecFromBootLayout(layout)
	sbx, err := m.startSandbox(ctx, namespace, req, vmStartSpec, runtimeIDs, resume)
	if err != nil {
		m.cleanupCreateFailure(sbx, req.SandboxId)
		return SandboxCreateResult{}, fmt.Errorf("failed to create sandbox: %w", err)
	}
	sbx.leaseID = leaseID

	entry.sbx = sbx
	entry.state = sandboxReady
	m.trackSandbox(ctx, mapKey, entry, req.SandboxId, sbx)
	cidAllocated = false

	logger.Debug("created sandbox in manager")
	return buildSandboxCreateResult(namespace, leaseID, req, sbx, layout, parentIDs, runtimeIDs, resume), nil
}

func (m *Manager) prepareRuntimeLease(ctx context.Context, namespace string, req SandboxCreateRequest) (context.Context, string, error) {
	leaseCtx := ctx
	leaseID := req.LeaseID
	if m.daemonClient == nil {
		return leaseCtx, leaseID, nil
	}
	leaseCtx, leaseID, err := m.daemonClient.WithRuntimeLease(ctx, namespace, leaseID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to ensure runtime lease: %w", err)
	}
	return leaseCtx, leaseID, nil
}

func (m *Manager) prepareParentSnapshots(ctx context.Context, namespace string, req SandboxCreateRequest) (snapshot.ParentSnapshotIDs, bool, error) {
	parentIDs, err := m.resolveParentSnapshotIDs(ctx, namespace, req)
	if err != nil {
		return snapshot.ParentSnapshotIDs{}, false, err
	}
	resume := req.SnapshotId != "" || req.UseSnapshot
	if resume && parentIDs.Mem == "" {
		return snapshot.ParentSnapshotIDs{}, false, fmt.Errorf("group mem ref label not found on rootfs snapshot %s", parentIDs.Rootfs)
	}
	return parentIDs, resume, nil
}

func (m *Manager) allocateCreateRuntimeIDs(req SandboxCreateRequest) (createRuntimeIDs, error) {
	key := req.SandboxId

	vsockSocketPath, err := SandboxVsockSocketPath(key)
	if err != nil {
		return createRuntimeIDs{}, fmt.Errorf("failed to create sandbox: vsock socket path error: %v", err)
	}

	vsockCID, err := m.AllocateUniqueCID(req.SandboxId)
	if err != nil {
		return createRuntimeIDs{}, fmt.Errorf("failed to create sandbox: CID allocation error: %v", err)
	}
	vcpuMax := req.VcpuMax
	if vcpuMax == 0 {
		vcpuMax = req.VcpuNum
	}
	return createRuntimeIDs{
		key:             key,
		vsockCID:        vsockCID,
		vsockSocketPath: vsockSocketPath,
		vcpuMax:         vcpuMax,
	}, nil
}

func (m *Manager) prepareBootLayout(ctx context.Context, namespace string, req SandboxCreateRequest, parentIDs snapshot.ParentSnapshotIDs, runtimeIDs createRuntimeIDs, resume bool) (*snapshot.BootLayout, error) {
	logger := ulog.GetLogger()
	var (
		layout *snapshot.BootLayout
		err    error
	)
	if resume {
		logger.Debug("creating sandbox by snapshotId")
		layout, err = m.snapshotServer.RestoreBootLayout(ctx, namespace, runtimeIDs.key, parentIDs, runtimeIDs.vsockCID, runtimeIDs.vsockSocketPath)
	} else {
		logger.Debug("creating sandbox by image", ulog.F("imageName", req.ImageName))
		layout, err = m.snapshotServer.CreateBootLayout(ctx, namespace, runtimeIDs.key, parentIDs, req.RamMB)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot layout: %w", err)
	}
	return layout, nil
}

func (m *Manager) startSandbox(ctx context.Context, namespace string, req SandboxCreateRequest, vmStartSpec VMStartSpec, runtimeIDs createRuntimeIDs, resume bool) (*Sandbox, error) {
	return createSandboxWithVsockSend(
		ctx,
		vmStartSpec,
		namespace,
		req.VmmName,
		req.SandboxId,
		req.AgentToken,
		req.VcpuNum,
		runtimeIDs.vcpuMax,
		m.pool,
		m.vsockSignalRetry,
		m.vsockSignalTimeout,
		resume,
		runtimeIDs.vsockCID,
		runtimeIDs.vsockSocketPath,
	)
}

func (m *Manager) cleanupCreateFailure(sbx *Sandbox, sandboxID string) {
	logger := ulog.GetLogger()
	if sbx != nil {
		if closeErr := sbx.Close(context.Background()); closeErr != nil {
			logger.Warn("failed to cleanup sandbox after create failure",
				ulog.F("sandbox_id", sandboxID),
				ulog.F("error", closeErr),
			)
		}
	}
}

func (m *Manager) trackSandbox(ctx context.Context, mapKey string, entry *sandboxEntry, sandboxID string, sbx *Sandbox) {
	logger := ulog.GetLogger()
	go func() {
		waitErr := sbx.Wait(ctx)
		if waitErr != nil {
			logger.Warn("failed to wait for sandbox, cleaning up", ulog.F("error", waitErr))
		}

		entry.mu.Lock()
		defer entry.mu.Unlock()
		if !m.isCurrentSandboxEntry(mapKey, entry) {
			return
		}
		if entry.state != sandboxReady || entry.sbx != sbx {
			return
		}

		entry.state = sandboxExited
		if err := m.cleanupSandbox(context.Background(), sbx, sandboxID); err != nil {
			logger.Warn("failed to cleanup sandbox after wait", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
		}
		m.sandboxes.CompareAndDelete(mapKey, entry)
	}()
}

func buildSandboxCreateResult(namespace, leaseID string, req SandboxCreateRequest, sbx *Sandbox, layout *snapshot.BootLayout, parentIDs snapshot.ParentSnapshotIDs, runtimeIDs createRuntimeIDs, resume bool) SandboxCreateResult {
	return SandboxCreateResult{
		IP:              sbx.slot.VpeerIPString(),
		AgentToken:      req.AgentToken,
		Namespace:       namespace,
		SandboxID:       req.SandboxId,
		LeaseID:         leaseID,
		VMMPID:          sbx.process.Pid(),
		VMMSocketPath:   sbx.process.VmmSocketPath,
		VsockCID:        runtimeIDs.vsockCID,
		VsockSocketPath: runtimeIDs.vsockSocketPath,
		NetworkSlotKey:  sbx.slot.Key,
		NetworkNS:       sbx.slot.NamespaceID(),
		RootfsKey:       runtimeIDs.key,
		MemKey:          snapshot.MemKeyFromRootfs(runtimeIDs.key),
		ParentRootfsID:  parentIDs.Rootfs,
		ParentMemID:     parentIDs.Mem,
		ParentVMID:      parentIDs.VM,
		RootfsMount:     layout.RootfsMount,
		MemMount:        layout.MemMount,
		VMMount:         layout.VMMount,
		RootDir:         layout.SnapshotDir,
		MemSize:         layout.MemorySizeMB,
		Resume:          resume,
	}
}

func (m *Manager) Rehydrate(records []state.SandboxRecord) (int, map[string]struct{}, error) {
	var (
		restored int
		errs     []error
	)
	restoredSandboxIDs := make(map[string]struct{})
	for _, rec := range records {
		if rec.State != state.SandboxReady {
			continue
		}
		if rec.ConchSandboxID == "" {
			continue
		}
		sb, err := attachSandboxFromRecord(rec, m.pool)
		if err != nil {
			errs = append(errs, fmt.Errorf("attach sandbox %s: %w", rec.PodSandboxID, err))
			continue
		}
		sb.leaseID = rec.LeaseID
		cidReserved := false
		if rec.VsockCID != 0 {
			if err := m.cidAllocator.ReserveCID(rec.ConchSandboxID, rec.VsockCID); err != nil {
				errs = append(errs, fmt.Errorf("reserve cid for %s: %w", rec.ConchSandboxID, err))
				continue
			}
			cidReserved = true
		}
		if sb.slot != nil && m.pool != nil {
			if err := m.pool.RestoreInUse(sb.slot, rec.ConchSandboxID, rec.IP); err != nil {
				if cidReserved {
					_ = m.ReleaseCID(rec.ConchSandboxID)
				}
				errs = append(errs, fmt.Errorf("restore network slot for %s: %w", rec.ConchSandboxID, err))
				continue
			}
		}
		m.sandboxes.Store(sandboxMapKey(rec.Namespace, rec.ConchSandboxID), &sandboxEntry{
			state: sandboxReady,
			sbx:   sb,
		})
		restoredSandboxIDs[rec.ConchSandboxID] = struct{}{}
		restored++
	}
	return restored, restoredSandboxIDs, errors.Join(errs...)
}

func (m *Manager) CleanupAssignedWithoutReadySandbox(restoredSandboxIDs map[string]struct{}) error {
	if m.pool != nil {
		return m.pool.CleanupAssignedWithoutReadySandbox(restoredSandboxIDs)
	}
	return nil
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

		if parents, ok, err := image.ResolveBootParentSnapshotIDs(ctx, m.daemonClient, namespace, req.ImageName); err != nil {
			return snapshot.ParentSnapshotIDs{}, fmt.Errorf("failed to resolve boot image snapshots: %w", err)
		} else if ok {
			return snapshot.ParentSnapshotIDs{
				Rootfs: parents.Rootfs,
				Mem:    parents.Mem,
				VM:     parents.VM,
			}, nil
		}

		var err error
		rootfsSnapshotID, err = image.GetSnapshotID(ctx, m.daemonClient, namespace, req.ImageName)
		if err != nil {
			return snapshot.ParentSnapshotIDs{}, fmt.Errorf("failed to resolve image snapshot: %w", err)
		}
	}

	parents, err := m.snapshotServer.ResolveImageParentSnapshotIDs(namespace, rootfsSnapshotID)
	if err != nil {
		return snapshot.ParentSnapshotIDs{}, err
	}
	return parents, nil
}

func (m *Manager) resolveNamespace(namespace string) string {
	if namespace != "" {
		return namespace
	}
	if m.daemonClient == nil {
		return "default"
	}
	return m.daemonClient.DefaultNamespace()
}

func (m *Manager) cleanupSandbox(ctx context.Context, sbx *Sandbox, sandboxID string) error {
	logger := ulog.GetLogger()
	var errs []error
	fields := []ulog.Field{
		ulog.F("sandbox_id", sandboxID),
		ulog.F("namespace", sbx.namespace),
		ulog.F("lease_id", sbx.leaseID),
	}

	finishClose := cleanupdiag.Start("sandbox.close", fields...)
	err := sbx.Close(ctx)
	finishClose(err)
	if err != nil {
		logger.Warn("failed to cleanup sandbox, will remove from cache",
			ulog.F("sandbox_id", sandboxID),
			ulog.F("error", err),
		)
		errs = append(errs, err)
	}

	snapshotCtx := ctx
	if sbx.leaseID != "" && m.daemonClient != nil {
		var leaseErr error
		finishLease := cleanupdiag.Start("sandbox.cleanup.restore_runtime_lease", fields...)
		snapshotCtx, _, leaseErr = m.daemonClient.WithRuntimeLease(ctx, sbx.namespace, sbx.leaseID)
		finishLease(leaseErr)
		if leaseErr != nil {
			logger.Warn("failed to restore runtime lease context for cleanup",
				ulog.F("sandbox_id", sandboxID),
				ulog.F("lease_id", sbx.leaseID),
				ulog.F("error", leaseErr),
			)
			errs = append(errs, leaseErr)
		}
	}
	finishSnapshot := cleanupdiag.Start("sandbox.snapshot.remove", fields...)
	err = m.snapshotServer.ReleaseBootLayout(snapshotCtx, sbx.namespace, sandboxID)
	finishSnapshot(err)
	if err != nil {
		logger.Warn("failed to remove sandbox snapshot",
			ulog.F("sandbox_id", sandboxID),
			ulog.F("namespace", sbx.namespace),
			ulog.F("error", err),
		)
		errs = append(errs, err)
	}

	finishCID := cleanupdiag.Start("sandbox.cid.release", fields...)
	releaseErr := m.ReleaseCID(sandboxID)
	finishCID(releaseErr)
	if releaseErr != nil {
		logger.Warn("failed to release CID", ulog.F("sandbox_id", sandboxID), ulog.F("error", releaseErr))
		errs = append(errs, releaseErr)
	}
	return errors.Join(errs...)
}

func (m *Manager) Delete(req SandboxDeleteRequest) error {
	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxId)
	entry, err := m.loadSandboxEntry(mapKey, req.SandboxId)
	if err != nil {
		return err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return nil
	}

	if entry.state != sandboxReady {
		return fmt.Errorf("sandbox %s is %s", req.SandboxId, entry.state)
	}
	sbx := entry.sbx
	if sbx == nil {
		return fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxId)
	}

	entry.state = sandboxDeleting
	err = m.cleanupSandbox(context.Background(), sbx, req.SandboxId)
	m.sandboxes.CompareAndDelete(mapKey, entry)
	return err
}

func (m *Manager) Pause(req SandboxPauseRequest) (string, error) {
	logger := ulog.GetLogger()

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxId)
	entry, err := m.loadSandboxEntry(mapKey, req.SandboxId)
	if err != nil {
		return "", err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return "", fmt.Errorf("sandbox %s not found", req.SandboxId)
	}
	if entry.state != sandboxReady {
		return "", fmt.Errorf("sandbox %s is %s", req.SandboxId, entry.state)
	}
	sbx := entry.sbx
	if sbx == nil {
		return "", fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxId)
	}
	entry.state = sandboxPausing

	leaseCtx := ctx
	if sbx.leaseID != "" && m.daemonClient != nil {
		var leaseErr error
		leaseCtx, _, leaseErr = m.daemonClient.WithRuntimeLease(ctx, namespace, sbx.leaseID)
		if leaseErr != nil {
			entry.state = sandboxReady
			return "", fmt.Errorf("failed to restore runtime lease context: %w", leaseErr)
		}
	}
	defer func() {
		logger.Info("sandbox stop in pause", ulog.F("sandboxId", req.SandboxId))
		if err := sbx.Stop(ctx); err != nil {
			logger.Error("sandbox stop error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
		if err := sbx.Close(ctx); err != nil {
			logger.Error("sandbox close error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
		if err := m.snapshotServer.ReleaseBootLayout(leaseCtx, sbx.namespace, req.SandboxId); err != nil {
			logger.Error("sandbox remove error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
		if releaseErr := m.ReleaseCID(req.SandboxId); releaseErr != nil {
			logger.Warn("failed to release CID after pause", ulog.F("sandbox_id", req.SandboxId), ulog.F("error", releaseErr))
		}
		m.sandboxes.CompareAndDelete(mapKey, entry)
	}()

	if err := sbx.Pause(ctx); err != nil {
		return "", fmt.Errorf("sandbox %s pause failed: %w", req.SandboxId, err)
	}

	// TODO: system sync, too large
	syscall.Sync()

	var key = req.SandboxId

	info, err := m.snapshotServer.SnapshotInfo(leaseCtx, sbx.namespace, key)
	if err != nil {
		return "", fmt.Errorf("failed to get snapshot info %s: %w", key, err)
	}
	parent := info.Parent
	snapshotId, err := snapshot.CalculateSnapshotID(sbx.namespace, key, parent)
	if err != nil {
		return "", fmt.Errorf("failed to calculate snapshot id: %w", err)
	}

	snapshotId, err = m.snapshotServer.CommitBootLayout(leaseCtx, sbx.namespace, snapshotId, key)
	if err != nil {
		return "", fmt.Errorf("failed to commit snapshot layout %s: %v", req.SandboxId, err)
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

func (m *Manager) WaitPoolPopulateStopped() {
	if m == nil || m.pool == nil {
		return
	}
	m.pool.WaitPopulateStopped()
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
