package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/agent/hostconn"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/vmm/driver"
	"github.com/openeuler/Conch/internal/volume"
	"github.com/openeuler/Conch/pkg/ulog"
)

type Manager struct {
	sandboxes          sync.Map // map[string]*sandboxEntry
	pool               *netstack.Pool
	daemonClient       *containerdclient.Client
	boot               BootPreparer
	checkpointCapture  CheckpointCapture
	vsockSignalRetry   time.Duration
	vsockSignalTimeout time.Duration
	requestTimeout     time.Duration
	cidAllocator       *CIDAllocator
	volumeManager      *volume.Manager
}

type sandboxLifecycleState uint8

const (
	sandboxReady sandboxLifecycleState = iota
	sandboxSuspended
)

func (s sandboxLifecycleState) String() string {
	switch s {
	case sandboxReady:
		return "ready"
	case sandboxSuspended:
		return "suspended"
	default:
		return "unknown"
	}
}

type sandboxEntry struct {
	mu    sync.Mutex
	state sandboxLifecycleState
	sbx   *Sandbox
}

func NewManager(p *netstack.Pool, daemonClient *containerdclient.Client, bootPreparer BootPreparer, vsockSignalRetry, vsockSignalTimeout, requestTimeout time.Duration) (*Manager, error) {
	if bootPreparer == nil {
		return nil, fmt.Errorf("sandbox boot preparer is required")
	}
	return &Manager{
		pool:               p,
		daemonClient:       daemonClient,
		boot:               bootPreparer,
		checkpointCapture:  NewFullCheckpointCapture(),
		vsockSignalRetry:   vsockSignalRetry,
		vsockSignalTimeout: vsockSignalTimeout,
		requestTimeout:     requestTimeout,
		cidAllocator:       NewCIDAllocator(),
	}, nil
}

func (m *Manager) SetVolumeManager(volumeManager *volume.Manager) {
	m.volumeManager = volumeManager
}

type CreateRequest struct {
	Namespace    string
	TemplateID   string
	VMMName      string
	SandboxID    string
	LeaseID      string
	VCPUNum      int64
	VCPUMax      int64
	RAMMB        int64
	AgentToken   string
	Env          map[string]string
	VolumeMounts []volume.Mount
}

type DeleteRequest struct {
	Namespace string
	SandboxID string
}

type LifecycleRequest struct {
	Namespace string
	SandboxID string
}

type CheckpointRequest struct {
	Namespace string
	SandboxID string
}

type CheckpointResult = CapturedBootComponents

type CreateResult struct {
	IP              string
	AgentToken      string
	Namespace       string
	SandboxID       string
	LeaseID         string
	VMMPID          int
	VMMSocketPath   string
	VsockCID        uint32
	VsockSocketPath string
	NetworkSlotID   int
	RootfsKey       string
	MemKey          string
	RootfsMount     string
	MemMount        string
	VMMount         string
	RootDir         string
	MemSize         int64
	Resume          bool
	BootIndexDigest string
	RootfsPmemPaths []string
	VolumeDevices   []volume.Device
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
	entry := &sandboxEntry{state: sandboxReady}
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
	existing.mu.Unlock()
	return "", nil, fmt.Errorf("sandbox %s already exists", sandboxID)
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

func createSandboxWithVsockSend(ctx context.Context, vmStartSpec VMStartSpec, namespace, vmmName, sandboxId, agentToken string, env map[string]string, vcpuNum, vcpuMax int64, pool *netstack.Pool, vsockSignalRetry, vsockSignalTimeout time.Duration, resume bool, vsockCID uint32, vsockSocketPath string) (*Sandbox, error) {
	logger := ulog.GetLogger()
	readyOpts := hostconn.ReadyOptions{
		SandboxID:       sandboxId,
		AgentToken:      agentToken,
		Env:             env,
		VMMName:         vmmName,
		VsockCID:        vsockCID,
		VsockSocketPath: vsockSocketPath,
		Retry:           vsockSignalRetry,
		Timeout:         vsockSignalTimeout,
	}
	if err := hostconn.ValidateReadyOptions(readyOpts); err != nil {
		return nil, err
	}

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
	conn, err := hostconn.WaitReady(ctx, readyOpts)
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

func (m *Manager) Create(req CreateRequest) (result CreateResult, err error) {
	logger := ulog.GetLogger()
	logger.Debug("creating sandbox in manager")

	if req.AgentToken == "" {
		return CreateResult{}, fmt.Errorf("agent token is required")
	}

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey, entry, err := m.reserveSandboxEntry(namespace, req.SandboxID)
	if err != nil {
		return CreateResult{}, err
	}
	defer entry.mu.Unlock()
	defer func() {
		if err != nil {
			m.sandboxes.CompareAndDelete(mapKey, entry)
		}
	}()

	leaseCtx, leaseID, err := m.prepareRuntimeLease(ctx, namespace, req)
	if err != nil {
		return CreateResult{}, err
	}

	runtimeIDs, err := m.allocateCreateRuntimeIDs(req)
	if err != nil {
		return CreateResult{}, err
	}
	cidAllocated := true
	defer func() {
		if err != nil && cidAllocated {
			if releaseErr := m.ReleaseCID(req.SandboxID); releaseErr != nil {
				logger.Warn("failed to release CID on create failure", ulog.F("sandbox_id", req.SandboxID), ulog.F("error", releaseErr))
			}
		}
	}()

	boot, err := m.prepareSandboxBoot(leaseCtx, namespace, req, runtimeIDs)
	if err != nil {
		return CreateResult{}, err
	}
	defer func() {
		if err == nil {
			return
		}
		rmErr := m.boot.Release(leaseCtx, ReleaseBootRequest{
			Namespace: namespace,
			SandboxID: runtimeIDs.key,
		})
		if rmErr != nil {
			logger.Error("failed to release sandbox boot layout", ulog.F("key", runtimeIDs.key), ulog.F("error", rmErr))
			return
		}
		logger.Info("released sandbox boot layout due to error", ulog.F("key", runtimeIDs.key))
	}()

	vmStartSpec := vmStartSpecFromBootSpec(boot.Spec)
	volumeDevices, err := m.prepareVolumes(namespace, req, boot.Runtime.Resume)
	if err != nil {
		return CreateResult{}, err
	}

	volumesPrepared := len(volumeDevices) > 0
	defer func() {
		if err == nil || !volumesPrepared || m.volumeManager == nil {
			return
		}
		if cleanupErr := m.volumeManager.CleanupSandbox(namespace, req.SandboxID, volumeDevices); cleanupErr != nil {
			logger.Warn("failed to cleanup volume mounts after create failure",
				ulog.F("sandbox_id", req.SandboxID),
				ulog.F("error", cleanupErr),
			)
		}
	}()
	vmStartSpec.VirtioFS = volumeDevicesToDriver(volumeDevices)
	sbx, err := m.startSandbox(ctx, namespace, req, vmStartSpec, runtimeIDs, boot.Runtime.Resume)
	if err != nil {
		m.cleanupCreateFailure(sbx, req.SandboxID)
		return CreateResult{}, fmt.Errorf("failed to create sandbox: %w", err)
	}
	sbx.leaseID = leaseID
	registerSandboxVolumeCleanup(sbx, m.volumeManager, namespace, req.SandboxID, volumeDevices)

	entry.sbx = sbx
	entry.state = sandboxReady
	m.trackSandbox(ctx, mapKey, entry, req.SandboxID, sbx)
	cidAllocated = false

	logger.Debug("created sandbox in manager")
	return buildSandboxCreateResult(namespace, leaseID, req, sbx, boot, runtimeIDs, volumeDevices), nil
}

func (m *Manager) prepareVolumes(namespace string, req CreateRequest, resume bool) ([]volume.Device, error) {
	if len(req.VolumeMounts) == 0 {
		return nil, nil
	}
	if resume {
		return nil, fmt.Errorf("sandbox with volumeMounts does not support snapshot startup")
	}
	if m.volumeManager == nil {
		return nil, fmt.Errorf("volume manager is not configured")
	}
	return m.volumeManager.PrepareSandbox(namespace, req.SandboxID, req.VolumeMounts)
}

func volumeDevicesToDriver(devices []volume.Device) []driver.VirtioFSDevice {
	if len(devices) == 0 {
		return nil
	}
	out := make([]driver.VirtioFSDevice, 0, len(devices))
	for _, device := range devices {
		out = append(out, driver.VirtioFSDevice{
			Tag:    device.Tag,
			Socket: device.Socket,
		})
	}
	return out
}

func (m *Manager) prepareRuntimeLease(ctx context.Context, namespace string, req CreateRequest) (context.Context, string, error) {
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

func (m *Manager) allocateCreateRuntimeIDs(req CreateRequest) (createRuntimeIDs, error) {
	key := req.SandboxID

	vsockSocketPath, err := SandboxVsockSocketPath(key)
	if err != nil {
		return createRuntimeIDs{}, fmt.Errorf("failed to create sandbox: vsock socket path error: %v", err)
	}

	vsockCID, err := m.AllocateUniqueCID(req.SandboxID)
	if err != nil {
		return createRuntimeIDs{}, fmt.Errorf("failed to create sandbox: CID allocation error: %v", err)
	}
	vcpuMax := req.VCPUMax
	if vcpuMax == 0 {
		vcpuMax = req.VCPUNum
	}
	return createRuntimeIDs{
		key:             key,
		vsockCID:        vsockCID,
		vsockSocketPath: vsockSocketPath,
		vcpuMax:         vcpuMax,
	}, nil
}

func (m *Manager) prepareSandboxBoot(ctx context.Context, namespace string, req CreateRequest, runtimeIDs createRuntimeIDs) (PreparedBoot, error) {
	if m.boot == nil {
		return PreparedBoot{}, fmt.Errorf("sandbox boot preparer is not configured")
	}
	logger := ulog.GetLogger()
	logger.Debug("preparing sandbox template", ulog.F("template_id", req.TemplateID))
	return m.boot.Prepare(ctx, PrepareBootRequest{
		Namespace:  namespace,
		TemplateID: req.TemplateID,
		SandboxID:  runtimeIDs.key,
		VMMName:    req.VMMName,
		RAMMB:      req.RAMMB,
	})
}

func (m *Manager) startSandbox(ctx context.Context, namespace string, req CreateRequest, vmStartSpec VMStartSpec, runtimeIDs createRuntimeIDs, resume bool) (*Sandbox, error) {
	return createSandboxWithVsockSend(
		ctx,
		vmStartSpec,
		namespace,
		req.VMMName,
		req.SandboxID,
		req.AgentToken,
		req.Env,
		req.VCPUNum,
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

		m.handleSandboxExit(mapKey, entry, sandboxID, sbx)
	}()
}

func (m *Manager) handleSandboxExit(mapKey string, entry *sandboxEntry, sandboxID string, sbx *Sandbox) {
	logger := ulog.GetLogger()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) || entry.sbx != sbx {
		return
	}

	if err := m.cleanupSandbox(context.Background(), sbx, sandboxID); err != nil {
		logger.Warn("failed to cleanup sandbox after wait", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
	}
	m.sandboxes.CompareAndDelete(mapKey, entry)
}

func buildSandboxCreateResult(namespace, leaseID string, req CreateRequest, sbx *Sandbox, boot PreparedBoot, runtimeIDs createRuntimeIDs, volumeDevices []volume.Device) CreateResult {
	runtime := boot.Runtime
	return CreateResult{
		IP:              sbx.slot.CNIIP(),
		AgentToken:      req.AgentToken,
		Namespace:       namespace,
		SandboxID:       req.SandboxID,
		LeaseID:         leaseID,
		VMMPID:          sbx.process.Pid(),
		VMMSocketPath:   sbx.process.VmmSocketPath,
		VsockCID:        runtimeIDs.vsockCID,
		VsockSocketPath: runtimeIDs.vsockSocketPath,
		NetworkSlotID:   sbx.slot.ID(),
		RootfsKey:       runtime.RootfsKey,
		MemKey:          runtime.MemKey,
		RootfsMount:     runtime.RootfsMount,
		MemMount:        runtime.MemMount,
		VMMount:         runtime.VMMount,
		RootDir:         runtime.RootDir,
		MemSize:         runtime.MemSize,
		Resume:          runtime.Resume,
		BootIndexDigest: runtime.BootIndexDigest,
		RootfsPmemPaths: append([]string(nil), boot.Spec.PmemPaths...),
		VolumeDevices:   append([]volume.Device(nil), volumeDevices...),
	}
}

func registerSandboxVolumeCleanup(sb *Sandbox, volumeManager *volume.Manager, namespace, sandboxID string, devices []volume.Device) {
	if sb == nil || volumeManager == nil || len(devices) == 0 {
		return
	}
	volumeDevices := append([]volume.Device(nil), devices...)
	sb.cleanup.Add(func(ctx context.Context) error {
		return volumeManager.CleanupSandbox(namespace, sandboxID, volumeDevices)
	})
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

	bootCtx := ctx
	if sbx.leaseID != "" && m.daemonClient != nil {
		var leaseErr error
		finishLease := cleanupdiag.Start("sandbox.cleanup.restore_runtime_lease", fields...)
		bootCtx, _, leaseErr = m.daemonClient.WithRuntimeLease(ctx, sbx.namespace, sbx.leaseID)
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
	finishBootRelease := cleanupdiag.Start("sandbox.boot.release", fields...)
	err = m.boot.Release(bootCtx, ReleaseBootRequest{
		Namespace: sbx.namespace,
		SandboxID: sandboxID,
	})
	finishBootRelease(err)
	if err != nil {
		logger.Warn("failed to release sandbox boot layout",
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

func (m *Manager) Delete(req DeleteRequest) error {
	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxID)
	entry, err := m.loadSandboxEntry(mapKey, req.SandboxID)
	if err != nil {
		return err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return nil
	}

	if entry.state != sandboxReady && entry.state != sandboxSuspended {
		return fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state)
	}
	sbx := entry.sbx
	if sbx == nil {
		return fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxID)
	}

	err = m.cleanupSandbox(context.Background(), sbx, req.SandboxID)
	m.sandboxes.CompareAndDelete(mapKey, entry)
	return err
}

func (m *Manager) Suspend(req LifecycleRequest) error {
	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxID)
	entry, err := m.loadSandboxEntry(mapKey, req.SandboxID)
	if err != nil {
		return err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return fmt.Errorf("sandbox %s not found", req.SandboxID)
	}
	if entry.state != sandboxReady {
		return fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state)
	}
	sbx := entry.sbx
	if sbx == nil {
		return fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxID)
	}

	if err := sbx.Suspend(ctx); err != nil {
		return fmt.Errorf("sandbox %s suspend failed: %w", req.SandboxID, err)
	}
	entry.state = sandboxSuspended
	return nil
}

func (m *Manager) Resume(req LifecycleRequest) error {
	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxID)
	entry, err := m.loadSandboxEntry(mapKey, req.SandboxID)
	if err != nil {
		return err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return fmt.Errorf("sandbox %s not found", req.SandboxID)
	}
	if entry.state != sandboxSuspended {
		return fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state)
	}
	sbx := entry.sbx
	if sbx == nil {
		return fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxID)
	}
	if err := sbx.Resume(ctx); err != nil {
		return fmt.Errorf("sandbox %s resume failed: %w", req.SandboxID, err)
	}
	entry.state = sandboxReady
	return nil
}

func (m *Manager) Checkpoint(req CheckpointRequest) (CheckpointResult, error) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxID)
	entry, err := m.loadSandboxEntry(mapKey, req.SandboxID)
	if err != nil {
		return CheckpointResult{}, err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return CheckpointResult{}, fmt.Errorf("sandbox %s not found", req.SandboxID)
	}
	wasSuspended := entry.state == sandboxSuspended
	if entry.state != sandboxReady && !wasSuspended {
		return CheckpointResult{}, fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state)
	}
	sbx := entry.sbx
	if sbx == nil {
		return CheckpointResult{}, fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxID)
	}
	if len(sbx.vmStartSpec.VirtioFS) > 0 {
		return CheckpointResult{}, fmt.Errorf("sandbox %s has volume mounts, checkpoint is not supported", req.SandboxID)
	}
	capture := m.checkpointCapture
	if capture == nil {
		capture = NewFullCheckpointCapture()
	}
	captured, err := capture.Capture(ctx, RuntimeCaptureRequest{
		Source:      sbx,
		PauseBefore: !wasSuspended,
	})
	if err != nil {
		if errors.Is(err, ErrCheckpointResume) {
			entry.state = sandboxSuspended
		}
		return CheckpointResult{}, fmt.Errorf("sandbox %s checkpoint failed: %w", req.SandboxID, err)
	}

	return captured, nil
}

func (m *Manager) Close() {
	if m == nil || m.pool == nil {
		return
	}
	m.pool.Close()
}

func (m *Manager) AllocateUniqueCID(sandboxId string) (uint32, error) {
	return m.cidAllocator.AllocateCID(sandboxId)
}

func (m *Manager) ReleaseCID(sandboxId string) error {
	return m.cidAllocator.ReleaseCID(sandboxId)
}
