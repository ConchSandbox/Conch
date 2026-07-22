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
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/internal/vmm/driver"
	"github.com/openeuler/Conch/internal/volume"
	"github.com/openeuler/Conch/pkg/ulog"
)

type Manager struct {
	sandboxes          sync.Map // map[string]*sandboxEntry
	pool               *netstack.Pool
	daemonClient       *containerdclient.Client
	template           TemplateManager
	checkpointCapture  CheckpointCapture
	vsockSignalRetry   time.Duration
	vsockSignalTimeout time.Duration
	requestTimeout     time.Duration
	cidAllocator       *CIDAllocator
	volumeManager      *volume.Manager
}

type sandboxLifecycleState uint8

const (
	sandboxCreating sandboxLifecycleState = iota
	sandboxReady
	sandboxSuspending
	sandboxSuspended
	sandboxResuming
	sandboxCheckpointing
	sandboxDeleting
	sandboxExited
)

func (s sandboxLifecycleState) String() string {
	switch s {
	case sandboxCreating:
		return "creating"
	case sandboxReady:
		return "ready"
	case sandboxSuspending:
		return "suspending"
	case sandboxSuspended:
		return "suspended"
	case sandboxResuming:
		return "resuming"
	case sandboxCheckpointing:
		return "checkpointing"
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

type TemplateManager interface {
	PrepareSandboxBoot(context.Context, template.PrepareSandboxBootRequest) (template.PreparedSandboxBoot, error)
	ReleaseSandboxBoot(context.Context, template.ReleaseSandboxBootRequest) error
}

func NewManager(p *netstack.Pool, daemonClient *containerdclient.Client, templateManager TemplateManager, vsockSignalRetry, vsockSignalTimeout, requestTimeout time.Duration) (*Manager, error) {
	if templateManager == nil {
		return nil, fmt.Errorf("template is required")
	}
	return &Manager{
		pool:               p,
		daemonClient:       daemonClient,
		template:           templateManager,
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

type SandboxCreateRequest struct {
	Namespace    string         `json:"namespace"`
	TemplateID   string         `json:"template_id"`
	VmmName      string         `json:"vmm_name"`
	SandboxId    string         `json:"sandbox_id"`
	LeaseID      string         `json:"lease_id,omitempty"`
	VcpuNum      int64          `json:"vcpu_num"`
	VcpuMax      int64          `json:"vcpu_max"`
	RamMB        int64          `json:"ram_mb"`
	AgentToken   string         `json:"-"`
	VolumeMounts []volume.Mount `json:"volumeMounts,omitempty"`
}

type SandboxDeleteRequest struct {
	Namespace string `json:"namespace"`
	SandboxId string `json:"sandbox_id"`
}

type SandboxLifecycleRequest struct {
	Namespace string `json:"namespace"`
	SandboxId string `json:"sandbox_id"`
}

type SandboxCheckpointRequest struct {
	Namespace string `json:"namespace"`
	SandboxId string `json:"sandbox_id"`
}

type SandboxCheckpointResult = CapturedBootComponents

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

	boot, err := m.prepareSandboxBoot(leaseCtx, namespace, req, runtimeIDs)
	if err != nil {
		return SandboxCreateResult{}, err
	}
	defer func() {
		if err == nil {
			return
		}
		rmErr := m.template.ReleaseSandboxBoot(leaseCtx, template.ReleaseSandboxBootRequest{
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
		return SandboxCreateResult{}, err
	}

	volumesPrepared := len(volumeDevices) > 0
	defer func() {
		if err == nil || !volumesPrepared || m.volumeManager == nil {
			return
		}
		if cleanupErr := m.volumeManager.CleanupSandbox(namespace, req.SandboxId, volumeDevices); cleanupErr != nil {
			logger.Warn("failed to cleanup volume mounts after create failure",
				ulog.F("sandbox_id", req.SandboxId),
				ulog.F("error", cleanupErr),
			)
		}
	}()
	vmStartSpec.VirtioFS = volumeDevicesToDriver(volumeDevices)
	sbx, err := m.startSandbox(ctx, namespace, req, vmStartSpec, runtimeIDs, boot.Runtime.Resume)
	if err != nil {
		m.cleanupCreateFailure(sbx, req.SandboxId)
		return SandboxCreateResult{}, fmt.Errorf("failed to create sandbox: %w", err)
	}
	sbx.leaseID = leaseID
	registerSandboxVolumeCleanup(sbx, m.volumeManager, namespace, req.SandboxId, volumeDevices)

	entry.sbx = sbx
	entry.state = sandboxReady
	m.trackSandbox(ctx, mapKey, entry, req.SandboxId, sbx)
	cidAllocated = false

	logger.Debug("created sandbox in manager")
	return buildSandboxCreateResult(namespace, leaseID, req, sbx, boot, runtimeIDs, volumeDevices), nil
}

func (m *Manager) prepareVolumes(namespace string, req SandboxCreateRequest, resume bool) ([]volume.Device, error) {
	if len(req.VolumeMounts) == 0 {
		return nil, nil
	}
	if resume {
		return nil, fmt.Errorf("sandbox with volumeMounts does not support snapshot startup")
	}
	if m.volumeManager == nil {
		return nil, fmt.Errorf("volume manager is not configured")
	}
	return m.volumeManager.PrepareSandbox(namespace, req.SandboxId, req.VolumeMounts)
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

func (m *Manager) prepareSandboxBoot(ctx context.Context, namespace string, req SandboxCreateRequest, runtimeIDs createRuntimeIDs) (template.PreparedSandboxBoot, error) {
	if m.template == nil {
		return template.PreparedSandboxBoot{}, fmt.Errorf("template is not configured")
	}
	logger := ulog.GetLogger()
	logger.Debug("preparing sandbox template", ulog.F("template_id", req.TemplateID))
	return m.template.PrepareSandboxBoot(ctx, template.PrepareSandboxBootRequest{
		Namespace:  namespace,
		TemplateID: req.TemplateID,
		SandboxID:  runtimeIDs.key,
		VMMName:    req.VmmName,
		RamMB:      req.RamMB,
	})
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

	entry.state = sandboxExited
	if err := m.cleanupSandbox(context.Background(), sbx, sandboxID); err != nil {
		logger.Warn("failed to cleanup sandbox after wait", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
	}
	m.sandboxes.CompareAndDelete(mapKey, entry)
}

func buildSandboxCreateResult(namespace, leaseID string, req SandboxCreateRequest, sbx *Sandbox, boot template.PreparedSandboxBoot, runtimeIDs createRuntimeIDs, volumeDevices []volume.Device) SandboxCreateResult {
	runtime := boot.Runtime
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

func (m *Manager) Rehydrate(records []state.SandboxRecord) (int, map[string]struct{}, error) {
	var (
		restored int
		errs     []error
	)
	restoredSandboxIDs := make(map[string]struct{})
	for _, rec := range records {
		if rec.State != state.SandboxReady {
			m.cleanupStaleVolumeState(rec, &errs)
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
		if rec.VsockCID != 0 {
			if err := m.cidAllocator.ReserveCID(rec.ConchSandboxID, rec.VsockCID); err != nil {
				if cleanupErr := m.cleanupSandbox(context.Background(), sb, rec.ConchSandboxID); cleanupErr != nil {
					err = errors.Join(err, cleanupErr)
				}
				errs = append(errs, fmt.Errorf("reserve cid for %s: %w", rec.ConchSandboxID, err))
				continue
			}
		}
		if sb.slot != nil && m.pool != nil {
			if err := m.pool.RestoreInUse(sb.slot, rec.ConchSandboxID, rec.IP); err != nil {
				if cleanupErr := m.cleanupSandbox(context.Background(), sb, rec.ConchSandboxID); cleanupErr != nil {
					err = errors.Join(err, cleanupErr)
				}
				errs = append(errs, fmt.Errorf("restore network slot for %s: %w", rec.ConchSandboxID, err))
				continue
			}
		}
		volumeDevices := volumeDevicesFromState(rec.VolumeDevices)
		if len(volumeDevices) > 0 && m.volumeManager == nil {
			err := fmt.Errorf("volume manager is not configured")
			if cleanupErr := m.cleanupSandbox(context.Background(), sb, rec.ConchSandboxID); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
			errs = append(errs, fmt.Errorf("restore volume state for %s: %w", rec.ConchSandboxID, err))
			continue
		}
		if len(volumeDevices) > 0 {
			namespace := rec.Namespace
			sandboxID := rec.ConchSandboxID
			volumeManager := m.volumeManager
			registerSandboxVolumeCleanup(sb, volumeManager, namespace, sandboxID, volumeDevices)
			if err := volumeManager.RestoreSandbox(namespace, sandboxID, volumeDevices); err != nil {
				if cleanupErr := m.cleanupSandbox(context.Background(), sb, sandboxID); cleanupErr != nil {
					err = errors.Join(err, cleanupErr)
				}
				errs = append(errs, fmt.Errorf("restore volume state for %s: %w", sandboxID, err))
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

func registerSandboxVolumeCleanup(sb *Sandbox, volumeManager *volume.Manager, namespace, sandboxID string, devices []volume.Device) {
	if sb == nil || volumeManager == nil || len(devices) == 0 {
		return
	}
	volumeDevices := append([]volume.Device(nil), devices...)
	sb.cleanup.Add(func(ctx context.Context) error {
		return volumeManager.CleanupSandbox(namespace, sandboxID, volumeDevices)
	})
}

func (m *Manager) cleanupStaleVolumeState(rec state.SandboxRecord, errs *[]error) {
	if m.volumeManager == nil || len(rec.VolumeDevices) == 0 {
		return
	}
	if processExists(rec.VMMPID) {
		return
	}
	namespace := m.resolveNamespace(rec.Namespace)
	sandboxID := rec.ConchSandboxID
	if sandboxID == "" {
		sandboxID = rec.PodSandboxID
	}
	if sandboxID == "" {
		return
	}
	if err := m.volumeManager.CleanupSandbox(namespace, sandboxID, volumeDevicesFromState(rec.VolumeDevices)); err != nil {
		*errs = append(*errs, fmt.Errorf("cleanup stale volume state for %s: %w", sandboxID, err))
	}
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return !errors.Is(err, syscall.ESRCH)
	}
	return true
}

func volumeDevicesFromState(devices []state.VolumeDevice) []volume.Device {
	if len(devices) == 0 {
		return nil
	}
	out := make([]volume.Device, 0, len(devices))
	for _, device := range devices {
		out = append(out, volume.Device{
			SandboxID:  device.SandboxID,
			Namespace:  device.Namespace,
			Backend:    device.Backend,
			Tag:        device.Tag,
			Socket:     device.Socket,
			VolumeDir:  device.VolumeDir,
			ConfigPath: device.ConfigPath,
			PID:        device.PID,
			StartTime:  device.StartTime,
		})
	}
	return out
}

func (m *Manager) CleanupAssignedWithoutReadySandbox(restoredSandboxIDs map[string]struct{}) error {
	if m.pool != nil {
		return m.pool.CleanupAssignedWithoutReadySandbox(restoredSandboxIDs)
	}
	return nil
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
	err = m.template.ReleaseSandboxBoot(bootCtx, template.ReleaseSandboxBootRequest{
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

	if entry.state != sandboxReady && entry.state != sandboxSuspended {
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

func (m *Manager) Suspend(req SandboxLifecycleRequest) error {
	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxId)
	entry, err := m.loadSandboxEntry(mapKey, req.SandboxId)
	if err != nil {
		return err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return fmt.Errorf("sandbox %s not found", req.SandboxId)
	}
	if entry.state != sandboxReady {
		return fmt.Errorf("sandbox %s is %s", req.SandboxId, entry.state)
	}
	sbx := entry.sbx
	if sbx == nil {
		return fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxId)
	}

	entry.state = sandboxSuspending
	if err := sbx.Suspend(ctx); err != nil {
		entry.state = sandboxReady
		return fmt.Errorf("sandbox %s suspend failed: %w", req.SandboxId, err)
	}
	entry.state = sandboxSuspended
	return nil
}

func (m *Manager) Resume(req SandboxLifecycleRequest) error {
	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxId)
	entry, err := m.loadSandboxEntry(mapKey, req.SandboxId)
	if err != nil {
		return err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return fmt.Errorf("sandbox %s not found", req.SandboxId)
	}
	if entry.state != sandboxSuspended {
		return fmt.Errorf("sandbox %s is %s", req.SandboxId, entry.state)
	}
	sbx := entry.sbx
	if sbx == nil {
		return fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxId)
	}
	entry.state = sandboxResuming
	if err := sbx.Resume(ctx); err != nil {
		entry.state = sandboxSuspended
		return fmt.Errorf("sandbox %s resume failed: %w", req.SandboxId, err)
	}
	entry.state = sandboxReady
	return nil
}

func (m *Manager) Checkpoint(req SandboxCheckpointRequest) (SandboxCheckpointResult, error) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxId)
	entry, err := m.loadSandboxEntry(mapKey, req.SandboxId)
	if err != nil {
		return SandboxCheckpointResult{}, err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox %s not found", req.SandboxId)
	}
	wasSuspended := entry.state == sandboxSuspended
	if entry.state != sandboxReady && !wasSuspended {
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox %s is %s", req.SandboxId, entry.state)
	}
	sbx := entry.sbx
	if sbx == nil {
		return SandboxCheckpointResult{}, fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxId)
	}
	if len(sbx.vmStartSpec.VirtioFS) > 0 {
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox %s has volume mounts, checkpoint is not supported", req.SandboxId)
	}
	previousState := entry.state
	entry.state = sandboxCheckpointing

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
		} else {
			entry.state = previousState
		}
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox %s checkpoint failed: %w", req.SandboxId, err)
	}

	entry.state = previousState
	return captured, nil
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
