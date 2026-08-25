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
	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/netstack"
	slotstate "github.com/openeuler/Conch/internal/netstack/slot"
	"github.com/openeuler/Conch/internal/vmm"
	"github.com/openeuler/Conch/internal/vmm/driver"
	"github.com/openeuler/Conch/internal/volume"
	"github.com/openeuler/Conch/pkg/ulog"
)

type Config struct {
	Network            netstack.PoolConfig
	VMMBinaries        map[string]string
	VsockSignalRetry   time.Duration
	VsockSignalTimeout time.Duration
	RequestTimeout     time.Duration
	VolumeManager      *volume.Manager
	PreGateEnabled     bool
	PreGateStateDir    string
}

type Manager struct {
	sandboxes             sync.Map // map[string]*sandboxEntry
	pool                  *netstack.Pool
	daemonClient          *containerdclient.Client
	boot                  BootPreparer
	checkpointCapture     CheckpointCapture
	vsockSignalRetry      time.Duration
	vsockSignalTimeout    time.Duration
	requestTimeout        time.Duration
	cidAllocator          *CIDAllocator
	volumeManager         *volume.Manager
	vmmBinaries           map[string]string
	preGateEnabled        bool
	preGateStateDir       string
	UnexpectedExitHandler UnexpectedExitHandler
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

type UnexpectedExitHandler func(sandboxID string)

func New(
	ctx context.Context,
	client *containerdclient.Client,
	templates TemplateReader,
	snapshots SnapshotBackend,
	cfg Config,
) (*Manager, error) {
	boot, err := NewBootPreparer(templates, snapshots, client, cfg.PreGateEnabled, cfg.PreGateStateDir)
	if err != nil {
		return nil, err
	}
	vsockSignalRetry := durationOrDefault(cfg.VsockSignalRetry, 10*time.Millisecond)
	vsockSignalTimeout := durationOrDefault(cfg.VsockSignalTimeout, 60*time.Second)
	requestTimeout := durationOrDefault(cfg.RequestTimeout, 60*time.Second)

	pool, err := netstack.NewPool(cfg.Network)
	if err != nil {
		return nil, err
	}
	manager, err := NewManager(pool, client, boot, vsockSignalRetry, vsockSignalTimeout, requestTimeout, cfg.VolumeManager, cfg.VMMBinaries)
	if err != nil {
		return nil, err
	}
	manager.preGateEnabled = cfg.PreGateEnabled
	manager.preGateStateDir = cfg.PreGateStateDir
	return manager, nil
}

// Start launches background warm network pool population. Callers should
// complete startup recovery first.
func (m *Manager) Start(ctx context.Context) error {
	if m == nil || m.pool == nil {
		return fmt.Errorf("sandbox manager is not initialized")
	}
	return m.pool.Start(ctx)
}

// RecoverStaleResources orchestrates cleanup of resources owned by a previous
// conchd process. It must run before Start so the new warm pool starts clean.
func (m *Manager) RecoverStaleResources(ctx context.Context, sandboxIDs []string, vmmPIDs []int, hasCreatingSandbox bool) error {
	if m == nil || m.pool == nil {
		return fmt.Errorf("sandbox manager is not initialized")
	}
	if err := vmm.CleanupStaleResources(vmmPIDs, m.vmmBinaries, hasCreatingSandbox); err != nil {
		return fmt.Errorf("clean stale VMM resources: %w", err)
	}
	if err := m.volumeManager.CleanupStaleResources(); err != nil {
		return fmt.Errorf("clean stale volume resources: %w", err)
	}
	if err := m.pool.CleanupStaleResources(ctx); err != nil {
		return fmt.Errorf("clean stale network resources: %w", err)
	}
	if err := m.cleanupStaleBootResources(ctx, sandboxIDs); err != nil {
		return fmt.Errorf("clean stale boot resources: %w", err)
	}
	return nil
}

func (m *Manager) cleanupStaleBootResources(ctx context.Context, sandboxIDs []string) error {
	if m == nil || m.boot == nil {
		return fmt.Errorf("sandbox boot preparer is not configured")
	}
	var errs []error
	for _, sandboxID := range sandboxIDs {
		if err := m.boot.Release(ctx, ReleaseBootRequest{SandboxID: sandboxID}); err != nil {
			errs = append(errs, fmt.Errorf("release sandbox %s boot layout: %w", sandboxID, err))
		}
	}
	return errors.Join(errs...)
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

func NewManager(
	p *netstack.Pool,
	daemonClient *containerdclient.Client,
	bootPreparer BootPreparer,
	vsockSignalRetry time.Duration,
	vsockSignalTimeout time.Duration,
	requestTimeout time.Duration,
	volumeManager *volume.Manager,
	vmmBinaries map[string]string,
) (*Manager, error) {
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
		volumeManager:      volumeManager,
		vmmBinaries:        cloneStringMap(vmmBinaries),
		cidAllocator:       NewCIDAllocator(),
	}, nil
}

func cloneStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	if m.pool != nil {
		m.pool.Close()
	}
	return nil
}

type CreateRequest struct {
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
	Network      *netstack.SandboxNetworkConfig
}

type DeleteRequest struct {
	SandboxID string
}

type LifecycleRequest struct {
	SandboxID string
}

type NetworkUpdateRequest struct {
	SandboxID string
	Network   *netstack.SandboxNetworkConfig
}

type CheckpointRequest struct {
	SandboxID string
}

type CheckpointResult = CapturedBootComponents

type CreateResult struct {
	IP              string
	AgentToken      string
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

func (m *Manager) reserveSandboxEntry(sandboxID string) (string, *sandboxEntry, error) {
	mapKey := sandboxID
	entry := &sandboxEntry{state: sandboxReady}
	entry.mu.Lock()

	actual, loaded := m.sandboxes.LoadOrStore(mapKey, entry)
	if !loaded {
		return mapKey, entry, nil
	}
	entry.mu.Unlock()

	_, ok := actual.(*sandboxEntry)
	if !ok {
		return "", nil, fmt.Errorf("invalid sandbox entry type for %s", sandboxID)
	}
	return "", nil, ErrAlreadyExists.Wrap(fmt.Errorf("sandbox %s already exists", sandboxID))
}

func (m *Manager) loadSandboxEntry(mapKey, sandboxID string) (*sandboxEntry, error) {
	entryVal, exists := m.sandboxes.Load(mapKey)
	if !exists {
		return nil, ErrNotFound.Wrap(fmt.Errorf("sandbox %s not found", sandboxID))
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

func (m *Manager) lockCurrentSandboxEntry(mapKey, sandboxID string) (*sandboxEntry, func(), error) {
	entry, err := m.loadSandboxEntry(mapKey, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	entry.mu.Lock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		entry.mu.Unlock()
		return nil, nil, ErrNotFound.Wrap(fmt.Errorf("sandbox %s not found", sandboxID))
	}
	return entry, entry.mu.Unlock, nil
}

func createSandboxWithVsockSend(ctx context.Context, vmStartSpec VMStartSpec, vmmName, vmmBinary, sandboxId, agentToken string, env map[string]string, vcpuNum, vcpuMax int64, pool *netstack.Pool, vsockSignalRetry, vsockSignalTimeout time.Duration, restore bool, vsockCID uint32, vsockSocketPath string, network *netstack.SandboxNetworkConfig) (*Sandbox, error) {
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
	if err := hostconn.ValidateReadyPreflight(readyOpts); err != nil {
		return nil, err
	}

	var sbx *Sandbox
	var createErr error
	if restore {
		sbx, createErr = RestoreSandbox(ctx, vmStartSpec, vmmName, vmmBinary, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath, network, &readyOpts)
	} else {
		sbx, createErr = CreateSandbox(ctx, vmStartSpec, vmmName, vmmBinary, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath, network, &readyOpts)
	}
	if createErr != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", createErr)
	}
	// WaitReady returns timeout and context cancellation errors directly.
	if err := hostconn.WaitReady(ctx, readyOpts); err != nil {
		return sbx, err
	}
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

	if m == nil {
		return CreateResult{}, fmt.Errorf("sandbox manager is not configured")
	}
	if _, ok := m.vmmBinaries[req.VMMName]; !ok {
		return CreateResult{}, ErrInvalidArgument.Wrap(fmt.Errorf("vmm %q is not configured", req.VMMName))
	}
	if req.AgentToken == "" {
		return CreateResult{}, fmt.Errorf("agent token is required")
	}

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	mapKey, entry, err := m.reserveSandboxEntry(req.SandboxID)
	if err != nil {
		return CreateResult{}, err
	}
	defer entry.mu.Unlock()
	defer func() {
		if err != nil {
			m.sandboxes.CompareAndDelete(mapKey, entry)
		}
	}()

	leaseCtx, leaseID, err := m.prepareRuntimeLease(ctx, req)
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

	boot, err := m.prepareSandboxBoot(leaseCtx, req, runtimeIDs)
	if err != nil {
		return CreateResult{}, err
	}
	defer func() {
		if err == nil {
			return
		}
		rmErr := m.boot.Release(leaseCtx, ReleaseBootRequest{
			SandboxID: runtimeIDs.key,
		})
		if rmErr != nil {
			logger.Error("failed to release sandbox boot layout", ulog.F("key", runtimeIDs.key), ulog.F("error", rmErr))
			return
		}
		logger.Info("released sandbox boot layout due to error", ulog.F("key", runtimeIDs.key))
	}()

	vmStartSpec := vmStartSpecFromBootSpec(boot.Spec)
	volumeDevices, err := m.prepareVolumes(req, boot.Runtime.Resume)
	if err != nil {
		return CreateResult{}, err
	}

	volumesPrepared := len(volumeDevices) > 0
	var virtiofsExit <-chan struct{}
	if volumesPrepared {
		virtiofsExit = volumeDevices[0].Exited
	}
	defer func() {
		if err == nil || !volumesPrepared || m.volumeManager == nil {
			return
		}
		if cleanupErr := m.volumeManager.CleanupSandbox(req.SandboxID, volumeDevices); cleanupErr != nil {
			logger.Warn("failed to cleanup volume mounts after create failure",
				ulog.F("sandbox_id", req.SandboxID),
				ulog.F("error", cleanupErr),
			)
		}
	}()
	vmStartSpec.VirtioFS = volumeDevicesToDriver(volumeDevices)
	sbx, err := m.startSandbox(ctx, req, vmStartSpec, runtimeIDs, boot.Runtime.Resume)
	if err != nil {
		m.cleanupCreateFailure(sbx, req.SandboxID)
		switch {
		case errors.Is(err, agentprotocol.ErrInvalidEnvironment):
			return CreateResult{}, ErrInvalidEnvironment.Wrap(err)
		case errors.Is(err, agentprotocol.ErrPayloadTooLarge):
			return CreateResult{}, ErrInitializationTooLarge.Wrap(err)
		case errors.Is(err, slotstate.ErrEmpty), errors.Is(err, slotstate.ErrCapacity):
			return CreateResult{}, ErrResourceExhausted.Wrap(err)
		}
		return CreateResult{}, fmt.Errorf("failed to create sandbox: %w", err)
	}
	sbx.leaseID = leaseID
	registerSandboxVolumeCleanup(sbx, m.volumeManager, req.SandboxID, volumeDevices)

	entry.sbx = sbx
	entry.state = sandboxReady
	m.trackSandbox(ctx, mapKey, entry, req.SandboxID, sbx, virtiofsExit)
	cidAllocated = false

	logger.Debug("created sandbox in manager")
	return buildSandboxCreateResult(leaseID, req, sbx, boot, runtimeIDs, volumeDevices), nil
}

func (m *Manager) prepareVolumes(req CreateRequest, resume bool) ([]volume.Device, error) {
	if len(req.VolumeMounts) == 0 {
		return nil, nil
	}
	if resume {
		return nil, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox with volumeMounts does not support snapshot startup"))
	}
	if m.volumeManager == nil {
		return nil, fmt.Errorf("volume manager is not configured")
	}
	return m.volumeManager.PrepareSandbox(req.SandboxID, req.VolumeMounts)
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

func (m *Manager) prepareRuntimeLease(ctx context.Context, req CreateRequest) (context.Context, string, error) {
	leaseCtx := ctx
	leaseID := req.LeaseID
	if m.daemonClient == nil {
		return leaseCtx, leaseID, nil
	}
	leaseCtx, leaseID, err := m.daemonClient.WithRuntimeLease(ctx, leaseID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to ensure runtime lease: %w", err)
	}
	return leaseCtx, leaseID, nil
}

func (m *Manager) allocateCreateRuntimeIDs(req CreateRequest) (createRuntimeIDs, error) {
	key := req.SandboxID

	vsockSocketPath, err := SandboxVsockSocketPath(key)
	if err != nil {
		return createRuntimeIDs{}, ErrInvalidArgument.Wrap(fmt.Errorf("invalid sandbox id for vsock socket path: %w", err))
	}

	vsockCID, err := m.AllocateUniqueCID(req.SandboxID)
	if err != nil {
		return createRuntimeIDs{}, ErrResourceExhausted.Wrap(fmt.Errorf("allocate sandbox CID: %w", err))
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

func (m *Manager) prepareSandboxBoot(ctx context.Context, req CreateRequest, runtimeIDs createRuntimeIDs) (PreparedBoot, error) {
	defer ulog.TraceCost(ulog.TraceStart(), req.SandboxID, "prepareSandboxBoot()")
	if m.boot == nil {
		return PreparedBoot{}, fmt.Errorf("sandbox boot preparer is not configured")
	}
	logger := ulog.GetLogger()
	logger.Debug("preparing sandbox template", ulog.F("template_id", req.TemplateID))
	return m.boot.Prepare(ctx, PrepareBootRequest{
		TemplateID: req.TemplateID,
		SandboxID:  runtimeIDs.key,
		VMMName:    req.VMMName,
		RAMMB:      req.RAMMB,
	})
}

func (m *Manager) startSandbox(ctx context.Context, req CreateRequest, vmStartSpec VMStartSpec, runtimeIDs createRuntimeIDs, restore bool) (*Sandbox, error) {
	vmmBinary, ok := m.vmmBinaries[req.VMMName]
	if !ok {
		return nil, fmt.Errorf("vmm %q is not configured", req.VMMName)
	}
	if restore && req.VMMName == vmm.StratovirtName && m.preGateEnabled {
		if err := configurePreGate(ctx, &vmStartSpec, req.SandboxID, m.preGateStateDir); err != nil {
			return nil, fmt.Errorf("configure pre-gate restore: %w", err)
		}
		defer cleanupResumeGate(vmStartSpec.ResumeGatePath)
	}
	return createSandboxWithVsockSend(
		ctx,
		vmStartSpec,
		req.VMMName,
		vmmBinary,
		req.SandboxID,
		req.AgentToken,
		req.Env,
		req.VCPUNum,
		runtimeIDs.vcpuMax,
		m.pool,
		m.vsockSignalRetry,
		m.vsockSignalTimeout,
		restore,
		runtimeIDs.vsockCID,
		runtimeIDs.vsockSocketPath,
		req.Network,
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

func (m *Manager) trackSandbox(ctx context.Context, mapKey string, entry *sandboxEntry, sandboxID string, sbx *Sandbox, virtiofsExit <-chan struct{}) {
	logger := ulog.GetLogger()
	go func() {
		vmmExit := make(chan struct{})
		go func() {
			waitErr := sbx.Wait(ctx)
			if waitErr != nil {
				logger.Warn("failed to wait for sandbox, cleaning up", ulog.F("error", waitErr))
			}
			close(vmmExit)
		}()

		select {
		case <-vmmExit:
		case <-virtiofsExit:
		}
		m.handleSandboxExit(mapKey, entry, sandboxID, sbx)
	}()
}

func (m *Manager) handleSandboxExit(mapKey string, entry *sandboxEntry, sandboxID string, sbx *Sandbox) {
	logger := ulog.GetLogger()
	entry.mu.Lock()
	if !m.isCurrentSandboxEntry(mapKey, entry) || entry.sbx != sbx {
		entry.mu.Unlock()
		return
	}
	if err := m.cleanupSandbox(context.Background(), sbx, sandboxID); err != nil {
		logger.Warn("failed to cleanup sandbox after wait", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
	}
	m.sandboxes.CompareAndDelete(mapKey, entry)
	entry.mu.Unlock()
	if m.UnexpectedExitHandler != nil {
		m.UnexpectedExitHandler(sandboxID)
	}
}

func buildSandboxCreateResult(leaseID string, req CreateRequest, sbx *Sandbox, boot PreparedBoot, runtimeIDs createRuntimeIDs, volumeDevices []volume.Device) CreateResult {
	runtime := boot.Runtime
	return CreateResult{
		IP:              sbx.slot.CNIIP(),
		AgentToken:      req.AgentToken,
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

func registerSandboxVolumeCleanup(sb *Sandbox, volumeManager *volume.Manager, sandboxID string, devices []volume.Device) {
	if sb == nil || volumeManager == nil || len(devices) == 0 {
		return
	}
	volumeDevices := append([]volume.Device(nil), devices...)
	sb.cleanup.Add(func(ctx context.Context) error {
		return volumeManager.CleanupSandbox(sandboxID, volumeDevices)
	})
}

func (m *Manager) cleanupSandbox(ctx context.Context, sbx *Sandbox, sandboxID string) error {
	logger := ulog.GetLogger()
	var errs []error
	fields := []ulog.Field{
		ulog.F("sandbox_id", sandboxID),
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
		bootCtx, _, leaseErr = m.daemonClient.WithRuntimeLease(ctx, sbx.leaseID)
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
		SandboxID: sandboxID,
	})
	finishBootRelease(err)
	if err != nil {
		logger.Warn("failed to release sandbox boot layout",
			ulog.F("sandbox_id", sandboxID),
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
	mapKey := req.SandboxID
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
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state))
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

	mapKey := req.SandboxID
	entry, unlock, err := m.lockCurrentSandboxEntry(mapKey, req.SandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if entry.state != sandboxReady {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state))
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

	mapKey := req.SandboxID
	entry, unlock, err := m.lockCurrentSandboxEntry(mapKey, req.SandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if entry.state != sandboxSuspended {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state))
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

func (m *Manager) UpdateNetwork(parent context.Context, req NetworkUpdateRequest) error {
	ctx, cancel := context.WithTimeoutCause(parent, m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	entry, unlock, err := m.lockCurrentSandboxEntry(req.SandboxID, req.SandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if entry.state != sandboxReady && entry.state != sandboxSuspended {
		return ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state))
	}
	if entry.sbx == nil || entry.sbx.slot == nil {
		return fmt.Errorf("invalid sandbox entry for %s: network slot is nil", req.SandboxID)
	}
	return m.pool.SetSandboxNetworkPolicy(ctx, entry.sbx.slot, req.SandboxID, req.Network)
}

func (m *Manager) Checkpoint(req CheckpointRequest) (CheckpointResult, error) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	mapKey := req.SandboxID
	entry, unlock, err := m.lockCurrentSandboxEntry(mapKey, req.SandboxID)
	if err != nil {
		return CheckpointResult{}, err
	}
	defer unlock()
	wasSuspended := entry.state == sandboxSuspended
	if entry.state != sandboxReady && !wasSuspended {
		return CheckpointResult{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state))
	}
	sbx := entry.sbx
	if sbx == nil {
		return CheckpointResult{}, fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxID)
	}
	if len(sbx.vmStartSpec.VirtioFS) > 0 {
		return CheckpointResult{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s has volume mounts, checkpoint is not supported", req.SandboxID))
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

func (m *Manager) AllocateUniqueCID(sandboxId string) (uint32, error) {
	return m.cidAllocator.AllocateCID(sandboxId)
}

func (m *Manager) ReleaseCID(sandboxId string) error {
	return m.cidAllocator.ReleaseCID(sandboxId)
}
