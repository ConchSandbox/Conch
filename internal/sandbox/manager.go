package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/agent/hostconn"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/vmm"
	"github.com/openeuler/Conch/internal/vmm/driver"
	"github.com/openeuler/Conch/internal/volume"
	"github.com/openeuler/Conch/internal/webhook"
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
	context            context.Context
	sandboxes          sync.Map // map[string]*sandboxEntry
	pool               *netstack.Pool
	boot               BootPreparer
	checkpointCapture  CheckpointCapture
	vsockSignalRetry   time.Duration
	vsockSignalTimeout time.Duration
	requestTimeout     time.Duration
	cidAllocator       *CIDAllocator
	volumeManager      *volume.Manager
	vmmBinaries        map[string]string
	store              Store
	client             *containerdclient.Client
	WebhookDispatcher  *webhook.Dispatcher
	lifecycleLocks     sandboxLifecycleLocks
	launch             func(context.Context, CreateRequest, VMStartSpec, createRuntimeIDs, bool) (*Sandbox, error)
}

const createCleanupTimeout = 10 * time.Second

type sandboxEntry struct {
	state   State
	sbx     *Sandbox
	volumes []volume.Device
}

func New(
	ctx context.Context,
	client *containerdclient.Client,
	snapshots SnapshotBackend,
	store Store,
	cfg Config,
) (*Manager, error) {
	if store == nil {
		return nil, fmt.Errorf("sandbox store is required")
	}
	requestTimeout := durationOrDefault(cfg.RequestTimeout, 60*time.Second)
	boot, err := NewBootPreparer(ctx, snapshots, client, requestTimeout)
	if err != nil {
		return nil, err
	}
	vsockSignalRetry := durationOrDefault(cfg.VsockSignalRetry, 10*time.Millisecond)
	vsockSignalTimeout := durationOrDefault(cfg.VsockSignalTimeout, 60*time.Second)

	pool, err := netstack.NewPool(cfg.Network)
	if err != nil {
		return nil, err
	}
	manager := &Manager{
		context: ctx, store: store, client: client, pool: pool, boot: boot,
		checkpointCapture: NewFullCheckpointCapture(),
		vsockSignalRetry:  vsockSignalRetry, vsockSignalTimeout: vsockSignalTimeout,
		requestTimeout: requestTimeout, volumeManager: cfg.VolumeManager,
		vmmBinaries: cloneStringMap(cfg.VMMBinaries), cidAllocator: NewCIDAllocator(),
	}
	manager.launch = manager.startSandbox
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
	TemplateName string
	VMMName      string
	SandboxID    string
	VCPUNum      int64
	VCPUMax      int64
	RAMMB        int64
	AgentToken   string
	Env          map[string]string
	VolumeMounts []volume.Mount
	Network      *netstack.SandboxNetworkConfig
}

type NetworkUpdateRequest struct {
	SandboxID string
	Network   *netstack.SandboxNetworkConfig
}

// Callers hold the sandbox's lifecycle lock.
func (m *Manager) loadSandboxEntry(sandboxID string) (*sandboxEntry, error) {
	value, ok := m.sandboxes.Load(sandboxID)
	if !ok {
		return nil, ErrNotFound.Wrap(fmt.Errorf("sandbox %s is not running", sandboxID))
	}
	return value.(*sandboxEntry), nil
}

type createRuntimeIDs struct {
	vsockCID        uint32
	vsockSocketPath string
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
	return createRuntimeIDs{
		vsockCID:        vsockCID,
		vsockSocketPath: vsockSocketPath,
	}, nil
}

func (m *Manager) prepareSandboxBoot(ctx context.Context, req CreateRequest) (PreparedBoot, error) {
	defer ulog.TraceCost(ulog.TraceStart(), req.SandboxID, "prepareSandboxBoot()")
	if m.boot == nil {
		return PreparedBoot{}, fmt.Errorf("sandbox boot preparer is not configured")
	}
	logger := ulog.GetLogger()
	logger.Debug("preparing sandbox template", ulog.F("template_id", req.TemplateID))
	return m.boot.Prepare(ctx, PrepareBootRequest{
		TemplateID: req.TemplateID,
		SandboxID:  req.SandboxID,
		VMMName:    req.VMMName,
		RAMMB:      req.RAMMB,
	})
}

func (m *Manager) startSandbox(ctx context.Context, req CreateRequest, vmStartSpec VMStartSpec, runtimeIDs createRuntimeIDs, restore bool) (*Sandbox, error) {
	logger := ulog.GetLogger()
	readyOpts := hostconn.ReadyOptions{
		SandboxID:       req.SandboxID,
		AgentToken:      req.AgentToken,
		Env:             req.Env,
		VMMName:         req.VMMName,
		VsockCID:        runtimeIDs.vsockCID,
		VsockSocketPath: runtimeIDs.vsockSocketPath,
		Retry:           m.vsockSignalRetry,
		Timeout:         m.vsockSignalTimeout,
	}
	if err := hostconn.ValidateReadyPreflight(readyOpts); err != nil {
		return nil, err
	}

	sbx, err := launchSandbox(ctx, req, vmStartSpec, m.vmmBinaries[req.VMMName], m.pool, runtimeIDs, &readyOpts, restore)
	if err != nil {
		return sbx, err
	}
	// WaitReady returns timeout and context cancellation errors directly.
	if err := hostconn.WaitReady(ctx, readyOpts); err != nil {
		return sbx, err
	}
	logger.Info("Vsock signal sent successfully", ulog.F("sandbox_id", req.SandboxID))
	return sbx, nil
}

func (m *Manager) trackSandbox(sandboxID string, entry *sandboxEntry, virtiofsExit <-chan struct{}) {
	processDone := entry.sbx.process.Done()
	go func() {
		select {
		case <-processDone:
		case <-virtiofsExit:
		case <-m.context.Done():
			return
		}
		m.handleSandboxExit(sandboxID, entry)
	}()
}

func (m *Manager) AllocateUniqueCID(sandboxId string) (uint32, error) {
	return m.cidAllocator.AllocateCID(sandboxId)
}

func (m *Manager) ReleaseCID(sandboxId string) error {
	return m.cidAllocator.ReleaseCID(sandboxId)
}
