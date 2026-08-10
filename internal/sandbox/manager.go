package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/agent/hostconn"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/cow"
	"github.com/openeuler/Conch/internal/memsnap"
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
	CowSocket          string
}

type Manager struct {
	sandboxes          sync.Map // map[string]*sandboxEntry
	pool               *netstack.Pool
	daemonClient       *containerdclient.Client
	boot               BootPreparer
	checkpointCapture  CheckpointCapture
	incrementalCapture CheckpointCapture
	vsockSignalRetry   time.Duration
	vsockSignalTimeout time.Duration
	requestTimeout     time.Duration
	cidAllocator       *CIDAllocator
	volumeManager      *volume.Manager
	vmmBinaries        map[string]string
	memory             MemoryAttacher
}

type MemoryAttacher interface {
	Attach(context.Context, cow.Request) (*os.File, cow.Response, error)
	WaitAttachmentReady(context.Context, string, string) (cow.Response, error)
	Detach(context.Context, string) (cow.Response, error)
}

type incrementalMemoryAttachment struct {
	client         MemoryAttacher
	file           *os.File
	token          string
	sandboxID      string
	uffdSocketPath string
	detached       bool
	mu             sync.Mutex
}

func (attachment *incrementalMemoryAttachment) waitReady(ctx context.Context) error {
	if attachment == nil || attachment.client == nil {
		return fmt.Errorf("incremental memory attachment is not configured")
	}
	_, err := attachment.client.WaitAttachmentReady(ctx, attachment.token, attachment.sandboxID)
	return err
}

func (attachment *incrementalMemoryAttachment) detach(ctx context.Context) error {
	if attachment == nil || attachment.client == nil {
		return nil
	}
	attachment.mu.Lock()
	defer attachment.mu.Unlock()
	if attachment.detached {
		return nil
	}
	_, err := attachment.client.Detach(ctx, attachment.token)
	if err == nil {
		attachment.detached = true
	}
	return err
}

func (attachment *incrementalMemoryAttachment) abort(ctx context.Context) error {
	if attachment == nil {
		return nil
	}
	var closeErr error
	if attachment.file != nil {
		closeErr = attachment.file.Close()
		attachment.file = nil
	}
	return errors.Join(closeErr, attachment.detach(ctx))
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
	manager.memory = cow.NewClient(cfg.CowSocket)
	incrementalCapture, err := NewIncrementalCheckpointCapture("")
	if err != nil {
		return nil, err
	}
	manager.incrementalCapture = incrementalCapture
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
	MemoryMode   string
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
	IP                 string
	AgentToken         string
	SandboxID          string
	LeaseID            string
	VMMPID             int
	VMMSocketPath      string
	VsockCID           uint32
	VsockSocketPath    string
	NetworkSlotID      int
	RootfsKey          string
	MemKey             string
	RootfsMount        string
	MemorySnapshotRoot string
	VMMount            string
	RootDir            string
	MemSize            int64
	Resume             bool
	BootIndexDigest    string
	RootfsPmemPaths    []string
	VolumeDevices      []volume.Device
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
	memoryAttachment, err := m.prepareIncrementalMemory(leaseCtx, req.SandboxID, boot)
	if err != nil {
		return CreateResult{}, fmt.Errorf("prepare incremental memory: %w", err)
	}
	defer func() {
		if err == nil || memoryAttachment == nil {
			return
		}
		_ = memoryAttachment.abort(context.WithoutCancel(leaseCtx))
	}()

	vmStartSpec := vmStartSpecFromBootSpec(boot.Spec)
	if memoryAttachment != nil {
		vmStartSpec.MemoryFile = memoryAttachment.file
		vmStartSpec.UFFDSocketPath = memoryAttachment.uffdSocketPath
		vmStartSpec.ResumeStartedHook = memoryAttachment.waitReady
	}
	volumeDevices, err := m.prepareVolumes(req, boot.Runtime.Resume)
	if err != nil {
		return CreateResult{}, err
	}

	volumesPrepared := len(volumeDevices) > 0
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
		return CreateResult{}, fmt.Errorf("failed to create sandbox: %w", err)
	}
	sbx.leaseID = leaseID
	if err := configureSandboxMemoryCapture(sbx, req, boot); err != nil {
		m.cleanupCreateFailure(sbx, req.SandboxID)
		return CreateResult{}, err
	}
	registerSandboxVolumeCleanup(sbx, m.volumeManager, req.SandboxID, volumeDevices)
	registerSandboxMemoryDetach(sbx, memoryAttachment)

	entry.sbx = sbx
	entry.state = sandboxReady
	m.trackSandbox(ctx, mapKey, entry, req.SandboxID, sbx)
	cidAllocated = false

	logger.Debug("created sandbox in manager")
	return buildSandboxCreateResult(leaseID, req, sbx, boot, runtimeIDs, volumeDevices), nil
}

func (m *Manager) prepareIncrementalMemory(ctx context.Context, sandboxID string, boot PreparedBoot) (*incrementalMemoryAttachment, error) {
	if !boot.Runtime.Resume || boot.Runtime.MemoryFormat != incrementalMemoryFormat {
		return nil, nil
	}
	if m.memory == nil {
		return nil, fmt.Errorf("cow client is required for incremental restore")
	}
	file, response, err := m.memory.Attach(ctx, cow.Request{MemorySnapshotRoot: boot.Runtime.MemorySnapshotRoot, SandboxID: sandboxID})
	attachment := &incrementalMemoryAttachment{
		client: m.memory, file: file, token: response.Token, sandboxID: sandboxID, uffdSocketPath: response.UFFDSocketPath,
	}
	if err != nil {
		return nil, errors.Join(err, attachment.abort(context.WithoutCancel(ctx)))
	}
	if file == nil || response.Token == "" || response.UFFDSocketPath == "" {
		return nil, errors.Join(fmt.Errorf("cow Attach returned incomplete attachment"), attachment.abort(context.WithoutCancel(ctx)))
	}
	const bytesPerMiB = uint64(1024 * 1024)
	if boot.Spec.MemorySizeMB <= 0 || response.MemorySize%bytesPerMiB != 0 || response.MemorySize/bytesPerMiB != uint64(boot.Spec.MemorySizeMB) {
		return nil, errors.Join(fmt.Errorf(
			"cow Attach memory size %d bytes does not match Boot Index memory size %d MiB",
			response.MemorySize,
			boot.Spec.MemorySizeMB,
		), attachment.abort(context.WithoutCancel(ctx)))
	}
	return attachment, nil
}

func registerSandboxMemoryDetach(sbx *Sandbox, attachment *incrementalMemoryAttachment) {
	if sbx == nil || sbx.cleanup == nil || attachment == nil {
		return
	}
	sbx.cleanup.Add(func(ctx context.Context) error { return attachment.detach(ctx) })
}

func configureSandboxMemoryCapture(sbx *Sandbox, req CreateRequest, boot PreparedBoot) error {
	if sbx == nil {
		return fmt.Errorf("sandbox is nil")
	}
	sbx.memoryMode = req.MemoryMode
	if req.MemoryMode != "incremental" {
		return nil
	}
	if req.VMMName != vmm.StratovirtName {
		return fmt.Errorf("incremental checkpoint requires StratoVirt")
	}
	sbx.memoryOrigin = "cold"
	if boot.Runtime.Resume {
		sbx.memoryOrigin = "restored"
		pinned, err := memsnap.LoadAndPin(boot.Runtime.MemorySnapshotRoot)
		if err != nil {
			return fmt.Errorf("load restored incremental manifest: %w", err)
		}
		sbx.memoryManifest = &pinned.Manifest
		sbx.memorySizeBytes = pinned.Manifest.MemorySize
		sbx.memoryBlockSize = pinned.Manifest.BlockSize
		if err := pinned.Close(); err != nil {
			return fmt.Errorf("close restored incremental manifest: %w", err)
		}
		return nil
	}
	if req.RAMMB <= 0 {
		return fmt.Errorf("incremental checkpoint requires positive guest RAM")
	}
	const bytesPerMiB = uint64(1024 * 1024)
	if uint64(req.RAMMB) > ^uint64(0)/bytesPerMiB {
		return fmt.Errorf("guest RAM size overflows bytes")
	}
	sbx.memorySizeBytes = uint64(req.RAMMB) * bytesPerMiB
	sbx.memoryBlockSize = memsnap.DefaultBlockSize
	return nil
}

func (m *Manager) prepareVolumes(req CreateRequest, resume bool) ([]volume.Device, error) {
	if len(req.VolumeMounts) == 0 {
		return nil, nil
	}
	if resume {
		return nil, fmt.Errorf("sandbox with volumeMounts does not support snapshot startup")
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

func buildSandboxCreateResult(leaseID string, req CreateRequest, sbx *Sandbox, boot PreparedBoot, runtimeIDs createRuntimeIDs, volumeDevices []volume.Device) CreateResult {
	runtime := boot.Runtime
	return CreateResult{
		IP:                 sbx.slot.CNIIP(),
		AgentToken:         req.AgentToken,
		SandboxID:          req.SandboxID,
		LeaseID:            leaseID,
		VMMPID:             sbx.process.Pid(),
		VMMSocketPath:      sbx.process.VmmSocketPath,
		VsockCID:           runtimeIDs.vsockCID,
		VsockSocketPath:    runtimeIDs.vsockSocketPath,
		NetworkSlotID:      sbx.slot.ID(),
		RootfsKey:          runtime.RootfsKey,
		MemKey:             runtime.MemKey,
		RootfsMount:        runtime.RootfsMount,
		MemorySnapshotRoot: runtime.MemorySnapshotRoot,
		VMMount:            runtime.VMMount,
		RootDir:            runtime.RootDir,
		MemSize:            runtime.MemSize,
		Resume:             runtime.Resume,
		BootIndexDigest:    runtime.BootIndexDigest,
		RootfsPmemPaths:    append([]string(nil), boot.Spec.PmemPaths...),
		VolumeDevices:      append([]volume.Device(nil), volumeDevices...),
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
	if sbx.CheckpointPoisoned() {
		return CheckpointResult{}, fmt.Errorf("sandbox %s cannot checkpoint after a previous incremental checkpoint failure", req.SandboxID)
	}
	capture := m.checkpointCapture
	if sbx.memoryMode == "incremental" {
		capture = m.incrementalCapture
		if capture == nil {
			return CheckpointResult{}, fmt.Errorf("incremental checkpoint capture is not configured")
		}
	} else if capture == nil {
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
	recordIncrementalManifest(sbx, captured)

	return captured, nil
}

func recordIncrementalManifest(sbx *Sandbox, captured CapturedBootComponents) {
	if sbx == nil || sbx.memoryOrigin != "restored" || captured.Manifest == nil {
		return
	}
	sbx.SetIncrementalManifest(*captured.Manifest)
}

func (m *Manager) CompleteCheckpoint(req LifecycleRequest) error {
	entry, unlock, err := m.lockCurrentSandboxEntry(req.SandboxID, req.SandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if entry.sbx == nil {
		return fmt.Errorf("invalid sandbox entry for %s: sandbox is nil", req.SandboxID)
	}
	entry.sbx.SetCheckpointPoisoned(false)
	return nil
}

func (m *Manager) AllocateUniqueCID(sandboxId string) (uint32, error) {
	return m.cidAllocator.AllocateCID(sandboxId)
}

func (m *Manager) ReleaseCID(sandboxId string) error {
	return m.cidAllocator.ReleaseCID(sandboxId)
}
