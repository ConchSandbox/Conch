package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/conchplugins"
	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/netstack"
	conchsandbox "github.com/openeuler/Conch/internal/sandbox"
	conchsnapshot "github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/pkg/ulog"
)

type Config struct {
	PoolSize           int                       `toml:"pool_size" json:"poolSize"`
	DynamicReservation bool                      `toml:"dynamic_reservation" json:"dynamicReservation"`
	BridgeCount        int                       `toml:"bridge_count" json:"bridgeCount"`
	TapIP              string                    `toml:"tap_ip" json:"tapIP"`
	TapMask            int                       `toml:"tap_mask" json:"tapMask"`
	CNI                netstack.CNIManagerConfig `toml:"cni" json:"cni"`
	VsockSignalRetry   string                    `toml:"vsock_signal_retry" json:"vsockSignalRetry"`
	VsockSignalTimeout string                    `toml:"vsock_signal_timeout" json:"vsockSignalTimeout"`
	RequestTimeout     string                    `toml:"request_timeout" json:"requestTimeout"`
}

type Service struct {
	manager  *conchsandbox.Manager
	closeMu  sync.Mutex
	closeErr error
	closed   bool
}

func New(ctx context.Context, client *containerdclient.Client, snapshotServer *conchsnapshot.Server, cfg Config) (*Service, error) {
	if snapshotServer == nil {
		return nil, fmt.Errorf("snapshot server is required")
	}
	vsockSignalRetry, err := parseDuration(cfg.VsockSignalRetry, 10*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("invalid vsock_signal_retry: %w", err)
	}
	vsockSignalTimeout, err := parseDuration(cfg.VsockSignalTimeout, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid vsock_signal_timeout: %w", err)
	}
	requestTimeout, err := parseDuration(cfg.RequestTimeout, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid request_timeout: %w", err)
	}

	pool, err := netstack.NewPool(cfg.PoolSize, cfg.DynamicReservation, cfg.BridgeCount, cfg.TapIP, cfg.TapMask, cfg.CNI, currentNetworkSlotStore())
	if err != nil {
		return nil, err
	}
	if _, err := pool.AdoptWarmIdle(ctx); err != nil {
		if errors.Is(err, netstack.ErrNetworkSlotStoreRead) || errors.Is(err, netstack.ErrNetworkSlotCleanup) {
			return nil, err
		}
		ulog.GetLogger().Warn("some warm network slots were not adopted", ulog.F("error", err))
	}
	manager, err := conchsandbox.NewManager(pool, client, snapshotServer, vsockSignalRetry, vsockSignalTimeout, requestTimeout)
	if err != nil {
		return nil, err
	}
	go pool.Populate(netstack.WithPreserveOnCancel(ctx))
	return &Service{manager: manager}, nil
}

func parseDuration(raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	return time.ParseDuration(raw)
}

func (s *Service) Create(req conchsandbox.SandboxCreateRequest) (conchsandbox.SandboxCreateResult, error) {
	return s.manager.Create(req)
}

func (s *Service) Delete(req conchsandbox.SandboxDeleteRequest) error {
	return s.manager.Delete(req)
}

func (s *Service) Pause(req conchsandbox.SandboxPauseRequest) (string, error) {
	return s.manager.Pause(req)
}

func (s *Service) UpdateNetwork(req conchsandbox.SandboxNetworkUpdateRequest) error {
	return s.manager.UpdateNetwork(req)
}

func (s *Service) Rehydrate(records []state.SandboxRecord) (int, map[string]struct{}, error) {
	if s == nil || s.manager == nil {
		return 0, nil, nil
	}
	return s.manager.Rehydrate(records)
}

func (s *Service) CleanupAssignedWithoutReadySandbox(restoredSandboxIDs map[string]struct{}) error {
	if s == nil || s.manager == nil {
		return nil
	}
	return s.manager.CleanupAssignedWithoutReadySandbox(restoredSandboxIDs)
}

func (s *Service) Close() error {
	if s == nil || s.manager == nil {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	finish := cleanupdiag.Start("sandbox_service.close_preserve")
	s.manager.WaitPoolPopulateStopped()
	finish(nil)
	return s.closeErr
}

var (
	readyMu          sync.Mutex
	readyCh          chan<- *Service
	networkSlotStore netstack.NetworkSlotStore
)

func SetReadyChannel(ch chan<- *Service) {
	readyMu.Lock()
	defer readyMu.Unlock()
	readyCh = ch
}

func SetNetworkSlotStore(store netstack.NetworkSlotStore) {
	readyMu.Lock()
	defer readyMu.Unlock()
	networkSlotStore = store
}

func currentNetworkSlotStore() netstack.NetworkSlotStore {
	readyMu.Lock()
	defer readyMu.Unlock()
	return networkSlotStore
}

func publishReady(svc *Service) {
	readyMu.Lock()
	ch := readyCh
	readyMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- svc:
	default:
	}
}

type daemonClientProvider interface {
	DaemonClient() *containerdclient.Client
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   conchplugins.SandboxServicePluginType,
		ID:     conchplugins.SandboxServiceID,
		Config: &Config{},
		Requires: []plugin.Type{
			conchplugins.HostPluginType,
			conchplugins.SnapshotServicePluginType,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			cfg := ic.Config.(*Config)
			inst, err := ic.GetByID(conchplugins.HostPluginType, conchplugins.HostPluginID)
			if err != nil {
				return nil, err
			}
			provider, ok := inst.(daemonClientProvider)
			if !ok {
				return nil, fmt.Errorf("%s does not provide daemon client", conchplugins.HostPluginURI)
			}
			snapshotInst, err := ic.GetByID(conchplugins.SnapshotServicePluginType, conchplugins.SnapshotServiceID)
			if err != nil {
				return nil, err
			}
			snapshotProvider, ok := snapshotInst.(snapshotSvc.ServerProvider)
			if !ok {
				return nil, fmt.Errorf("%s does not provide snapshot server", conchplugins.SnapshotServiceURI)
			}
			svc, err := New(ic.Context, provider.DaemonClient(), snapshotProvider.SnapshotServer(), *cfg)
			if err != nil {
				return nil, err
			}
			publishReady(svc)
			return svc, nil
		},
	})
}
