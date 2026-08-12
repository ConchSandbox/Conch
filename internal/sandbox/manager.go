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
}

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
	vmmBinaries        map[string]string
}

type sandboxLifecycleState uint8

const (
	sandboxCreating sandboxLifecycleState = iota
	sandboxReady
	sandboxSuspended
	sandboxStopping
	sandboxExited
)

func (s sandboxLifecycleState) String() string {
	switch s {
	case sandboxCreating:
		return "creating"
	case sandboxReady:
		return "ready"
	case sandboxSuspended:
		return "suspended"
	case sandboxStopping:
		return "stopping"
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

	dependencyErr error
	cleanupDone   chan struct{}
	cleanupErr    error
}

func New(
	ctx context.Context,
	client *containerdclient.Client,
	templates TemplateReader,
	snapshots SnapshotBackend,
	cfg Config,
) (*Manager, error) {
	boot, err := NewBootPreparer(templates, snapshots, client)
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
func (m *Manager) RecoverStaleResources(ctx context.Context, sandboxIDs []string) error {
	if m == nil || m.pool == nil {
		return fmt.Errorf("sandbox manager is not initialized")
	}
	if err := vmm.CleanupStaleResources(); err != nil {
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
	entry := &sandboxEntry{
		state:       sandboxCreating,
		cleanupDone: make(chan struct{}),
	}

	actual, loaded := m.sandboxes.LoadOrStore(mapKey, entry)
	if !loaded {
		return mapKey, entry, nil
	}

	_, ok := actual.(*sandboxEntry)
	if !ok {
		return "", nil, fmt.Errorf("invalid sandbox entry type for %s", sandboxID)
	}
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

func (m *Manager) lockCurrentSandboxEntry(mapKey, sandboxID string) (*sandboxEntry, func(), error) {
	entry, err := m.loadSandboxEntry(mapKey, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	entry.mu.Lock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		entry.mu.Unlock()
		return nil, nil, fmt.Errorf("sandbox %s not found", sandboxID)
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
		sbx, createErr = RestoreSandbox(ctx, vmStartSpec, vmmName, vmmBinary, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath, network)
	} else {
		sbx, createErr = CreateSandbox(ctx, vmStartSpec, vmmName, vmmBinary, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath, network)
	}
	if createErr != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", createErr)
	}
	readyOpts.Network = sbx.slot.GuestNetworkConfig()
	if err := readyOpts.Network.Validate(); err != nil {
		return sbx, fmt.Errorf("invalid guest network config: %w", err)
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

	if req.AgentToken == "" {
		return CreateResult{}, fmt.Errorf("agent token is required")
	}

	timeoutCtx, timeoutCancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer timeoutCancel()
	ctx, cancelCreate := context.WithCancelCause(timeoutCtx)
	defer cancelCreate(nil)

	mapKey, entry, err := m.reserveSandboxEntry(req.SandboxID)
	if err != nil {
		return CreateResult{}, err
	}

	leaseCtx := context.Context(ctx)
	var runtimeIDs createRuntimeIDs
	var cidAllocated bool
	var boot PreparedBoot
	var bootPrepared bool
	var prepared volume.PreparedSandbox
	var volumeOwnedByCreate bool
	var sbx *Sandbox

	defer func() {
		if err == nil {
			return
		}

		dependencyErr := m.markCreatingEntryStopping(mapKey, entry)
		if dependencyErr != nil && !errors.Is(err, dependencyErr) {
			err = errors.Join(dependencyErr, err)
		}

		var cleanupErrs []error
		if sbx != nil {
			if closeErr := sbx.Close(context.Background()); closeErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("cleanup sandbox after create failure: %w", closeErr))
				logger.Warn("failed to cleanup sandbox after create failure",
					ulog.F("sandbox_id", req.SandboxID),
					ulog.F("error", closeErr),
				)
			}
		}
		if volumeOwnedByCreate && m.volumeManager != nil {
			if cleanupErr := m.volumeManager.CleanupSandbox(req.SandboxID, prepared); cleanupErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("cleanup volumes after create failure: %w", cleanupErr))
				logger.Warn("failed to cleanup volume mounts after create failure",
					ulog.F("sandbox_id", req.SandboxID),
					ulog.F("error", cleanupErr),
				)
			}
		}
		if bootPrepared {
			if releaseErr := m.boot.Release(context.WithoutCancel(leaseCtx), ReleaseBootRequest{SandboxID: runtimeIDs.key}); releaseErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("release sandbox boot layout: %w", releaseErr))
				logger.Error("failed to release sandbox boot layout", ulog.F("key", runtimeIDs.key), ulog.F("error", releaseErr))
			}
		}
		if cidAllocated {
			if releaseErr := m.ReleaseCID(req.SandboxID); releaseErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("release CID: %w", releaseErr))
				logger.Warn("failed to release CID on create failure", ulog.F("sandbox_id", req.SandboxID), ulog.F("error", releaseErr))
			}
		}

		cleanupErr := errors.Join(cleanupErrs...)
		m.finishCreatingFailure(mapKey, entry, cleanupErr)
		err = errors.Join(err, cleanupErr)
	}()

	var leaseID string
	leaseCtx, leaseID, err = m.prepareRuntimeLease(ctx, req)
	if err != nil {
		return CreateResult{}, err
	}

	runtimeIDs, err = m.allocateCreateRuntimeIDs(req)
	if err != nil {
		return CreateResult{}, err
	}
	cidAllocated = true

	boot, err = m.prepareSandboxBoot(leaseCtx, req, runtimeIDs)
	if err != nil {
		return CreateResult{}, err
	}
	bootPrepared = true

	vmStartSpec := vmStartSpecFromBootSpec(boot.Spec)
	prepared, err = m.prepareVolumes(req, boot.Runtime.Resume)
	if err != nil {
		return CreateResult{}, err
	}
	volumeOwnedByCreate = prepared.Watch != nil || len(prepared.Devices) > 0
	m.watchVolumeProcess(mapKey, entry, prepared.Watch, cancelCreate)

	vmStartSpec.VirtioFS = volumeDevicesToDriver(prepared.Devices)
	sbx, err = m.startSandbox(ctx, req, vmStartSpec, runtimeIDs, boot.Runtime.Resume)
	if err != nil {
		return CreateResult{}, fmt.Errorf("failed to create sandbox: %w", err)
	}
	sbx.leaseID = leaseID
	if registerSandboxVolumeCleanup(sbx, m.volumeManager, req.SandboxID, prepared) {
		volumeOwnedByCreate = false
	}

	result = buildSandboxCreateResult(leaseID, req, sbx, boot, runtimeIDs, prepared.Devices)
	if err := m.commitReady(mapKey, entry, sbx); err != nil {
		return CreateResult{}, err
	}
	m.trackSandbox(ctx, mapKey, entry, req.SandboxID, sbx)
	cidAllocated = false

	logger.Debug("created sandbox in manager")
	return result, nil
}

func (m *Manager) prepareVolumes(req CreateRequest, resume bool) (volume.PreparedSandbox, error) {
	if len(req.VolumeMounts) == 0 {
		return volume.PreparedSandbox{}, nil
	}
	if resume {
		return volume.PreparedSandbox{}, fmt.Errorf("sandbox with volumeMounts does not support snapshot startup")
	}
	if m.volumeManager == nil {
		return volume.PreparedSandbox{}, fmt.Errorf("volume manager is not configured")
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

func (m *Manager) prepareSandboxBoot(ctx context.Context, req CreateRequest, runtimeIDs createRuntimeIDs) (PreparedBoot, error) {
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

func (m *Manager) commitReady(mapKey string, entry *sandboxEntry, sbx *Sandbox) error {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return fmt.Errorf("sandbox %s is no longer current", mapKey)
	}
	if entry.dependencyErr != nil {
		return entry.dependencyErr
	}
	if entry.state != sandboxCreating {
		return fmt.Errorf("sandbox %s is %s", mapKey, entry.state)
	}
	if sbx == nil {
		return fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", mapKey)
	}
	entry.sbx = sbx
	entry.state = sandboxReady
	return nil
}

func (m *Manager) markCreatingEntryStopping(mapKey string, entry *sandboxEntry) error {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return nil
	}
	if entry.state == sandboxCreating {
		entry.state = sandboxStopping
	}
	return entry.dependencyErr
}

func (m *Manager) finishCreatingFailure(mapKey string, entry *sandboxEntry, cleanupErr error) {
	entry.mu.Lock()
	entry.cleanupErr = cleanupErr
	if entry.state != sandboxExited {
		entry.state = sandboxExited
		if entry.cleanupDone == nil {
			entry.cleanupDone = make(chan struct{})
		}
		close(entry.cleanupDone)
	}
	entry.mu.Unlock()
	m.sandboxes.CompareAndDelete(mapKey, entry)
}

func (m *Manager) watchVolumeProcess(
	mapKey string,
	entry *sandboxEntry,
	watch *volume.ProcessWatch,
	cancelCreate context.CancelCauseFunc,
) {
	if watch == nil || watch.Done() == nil {
		return
	}
	go func() {
		<-watch.Done()
		result, ok := watch.Result()
		if !ok {
			ulog.GetLogger().Warn("virtiofsd monitor completed without a result", ulog.F("sandbox_id", mapKey))
			return
		}
		m.handleVolumeProcessObservation(mapKey, entry, result, cancelCreate)
	}()
}

func (m *Manager) handleVolumeProcessObservation(
	mapKey string,
	entry *sandboxEntry,
	result volume.ProcessObservation,
	cancelCreate context.CancelCauseFunc,
) {
	dependencyErr := volumeProcessObservationError(result)

	entry.mu.Lock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		entry.mu.Unlock()
		return
	}
	switch entry.state {
	case sandboxCreating:
		if entry.dependencyErr == nil {
			entry.dependencyErr = dependencyErr
		}
		dependencyErr = entry.dependencyErr
		entry.mu.Unlock()
		if cancelCreate != nil {
			cancelCreate(dependencyErr)
		}
		return
	case sandboxReady, sandboxSuspended:
		entry.mu.Unlock()
	default:
		entry.mu.Unlock()
		return
	}

	sbx, _, owner, err := m.beginStopping(mapKey, entry, nil)
	if err != nil || !owner {
		return
	}
	logger := ulog.GetLogger()
	logger.Warn("virtiofsd observation completed while sandbox was active",
		ulog.F("sandbox_id", mapKey),
		ulog.F("pid", result.PID),
		ulog.F("exited", result.Exited),
		ulog.F("error", result.Cause),
	)
	cleanupErr := m.cleanupSandbox(context.Background(), sbx, mapKey)
	if cleanupErr != nil {
		logger.Warn("failed to cleanup sandbox after virtiofsd observation",
			ulog.F("sandbox_id", mapKey),
			ulog.F("error", cleanupErr),
		)
	}
	m.finishStopping(mapKey, entry, cleanupErr)
}

func volumeProcessObservationError(result volume.ProcessObservation) error {
	message := fmt.Sprintf("virtiofsd exited while sandbox was active (pid %d)", result.PID)
	if !result.Exited {
		message = fmt.Sprintf("virtiofsd observer failed while sandbox was active (pid %d)", result.PID)
	}
	if result.Signal != "" {
		message += fmt.Sprintf(" after signal %s", result.Signal)
	} else if result.ExitCode != nil {
		message += fmt.Sprintf(" with exit code %d", *result.ExitCode)
	}
	if result.Cause != nil {
		return fmt.Errorf("%s: %w", message, result.Cause)
	}
	return errors.New(message)
}

func (m *Manager) beginStopping(
	mapKey string,
	entry *sandboxEntry,
	expected *Sandbox,
) (sbx *Sandbox, done <-chan struct{}, owner bool, err error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !m.isCurrentSandboxEntry(mapKey, entry) {
		return nil, nil, false, nil
	}
	if expected != nil && entry.sbx != expected {
		return nil, nil, false, nil
	}
	if entry.cleanupDone == nil {
		entry.cleanupDone = make(chan struct{})
	}
	switch entry.state {
	case sandboxReady, sandboxSuspended:
		if entry.sbx == nil {
			return nil, nil, false, fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", mapKey)
		}
		entry.state = sandboxStopping
		return entry.sbx, entry.cleanupDone, true, nil
	case sandboxStopping, sandboxExited:
		return entry.sbx, entry.cleanupDone, false, nil
	default:
		return nil, nil, false, fmt.Errorf("sandbox %s is %s", mapKey, entry.state)
	}
}

func (m *Manager) finishStopping(mapKey string, entry *sandboxEntry, cleanupErr error) {
	entry.mu.Lock()
	if entry.state == sandboxStopping {
		entry.cleanupErr = cleanupErr
		entry.state = sandboxExited
		if entry.cleanupDone == nil {
			entry.cleanupDone = make(chan struct{})
		}
		close(entry.cleanupDone)
	}
	entry.mu.Unlock()
	m.sandboxes.CompareAndDelete(mapKey, entry)
}

func cleanupResult(entry *sandboxEntry) error {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.cleanupErr
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
	ownedSandbox, _, owner, err := m.beginStopping(mapKey, entry, sbx)
	if err != nil || !owner {
		return
	}

	cleanupErr := m.cleanupSandbox(context.Background(), ownedSandbox, sandboxID)
	if cleanupErr != nil {
		logger.Warn("failed to cleanup sandbox after wait", ulog.F("sandbox_id", sandboxID), ulog.F("error", cleanupErr))
	}
	m.finishStopping(mapKey, entry, cleanupErr)
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

func registerSandboxVolumeCleanup(sb *Sandbox, volumeManager *volume.Manager, sandboxID string, prepared volume.PreparedSandbox) bool {
	if sb == nil || sb.cleanup == nil || volumeManager == nil || (len(prepared.Devices) == 0 && prepared.Watch == nil) {
		return false
	}
	volumePrepared := prepared
	volumePrepared.Devices = append([]volume.Device(nil), prepared.Devices...)
	sb.cleanup.Add(func(ctx context.Context) error {
		return volumeManager.CleanupSandbox(sandboxID, volumePrepared)
	})
	return true
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

	sbx, done, owner, err := m.beginStopping(mapKey, entry, nil)
	if err != nil {
		return err
	}
	if !owner {
		if done != nil {
			<-done
		}
		return cleanupResult(entry)
	}

	cleanupErr := m.cleanupSandbox(context.Background(), sbx, req.SandboxID)
	m.finishStopping(mapKey, entry, cleanupErr)
	return cleanupErr
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

	mapKey := req.SandboxID
	entry, unlock, err := m.lockCurrentSandboxEntry(mapKey, req.SandboxID)
	if err != nil {
		return err
	}
	defer unlock()
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

func (m *Manager) UpdateNetwork(parent context.Context, req NetworkUpdateRequest) error {
	ctx, cancel := context.WithTimeoutCause(parent, m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	entry, unlock, err := m.lockCurrentSandboxEntry(req.SandboxID, req.SandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if entry.state != sandboxReady && entry.state != sandboxSuspended {
		return fmt.Errorf("sandbox %s is %s", req.SandboxID, entry.state)
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

func (m *Manager) AllocateUniqueCID(sandboxId string) (uint32, error) {
	return m.cidAllocator.AllocateCID(sandboxId)
}

func (m *Manager) ReleaseCID(sandboxId string) error {
	return m.cidAllocator.ReleaseCID(sandboxId)
}
