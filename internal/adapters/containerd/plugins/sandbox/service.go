package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/conchplugins"
	"github.com/openeuler/Conch/internal/netstack"
	conchsandbox "github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/volume"
)

type Config struct {
	WarmPoolSize       int                       `toml:"warm_pool_size" json:"warmPoolSize"`
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

func New(
	ctx context.Context,
	client *containerdclient.Client,
	templates conchsandbox.TemplateReader,
	snapshots conchsandbox.SnapshotBackend,
	resolver conchsandbox.BootResolver,
	cfg Config,
) (*Service, error) {
	boot, err := conchsandbox.NewBootPreparer(templates, snapshots, resolver)
	if err != nil {
		return nil, err
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

	pool, err := netstack.NewPool(cfg.WarmPoolSize, cfg.TapIP, cfg.TapMask, cfg.CNI)
	if err != nil {
		return nil, err
	}
	manager, err := conchsandbox.NewManager(pool, client, boot, vsockSignalRetry, vsockSignalTimeout, requestTimeout)
	if err != nil {
		return nil, err
	}
	pool.Start(ctx)
	return &Service{manager: manager}, nil
}

func parseDuration(raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	return time.ParseDuration(raw)
}

func (s *Service) Create(req conchsandbox.CreateRequest) (conchsandbox.CreateResult, error) {
	return s.manager.Create(req)
}

func (s *Service) Delete(req conchsandbox.DeleteRequest) error {
	return s.manager.Delete(req)
}

func (s *Service) Suspend(req conchsandbox.LifecycleRequest) error {
	return s.manager.Suspend(req)
}

func (s *Service) Resume(req conchsandbox.LifecycleRequest) error {
	return s.manager.Resume(req)
}

func (s *Service) Checkpoint(req conchsandbox.CheckpointRequest) (conchsandbox.CheckpointResult, error) {
	return s.manager.Checkpoint(req)
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	finish := cleanupdiag.Start("sandbox_service.close")
	if s.manager != nil {
		s.manager.Close()
	}
	finish(nil)
	return s.closeErr
}

var (
	readyMu       sync.Mutex
	readyCh       chan<- *Service
	volumeManager *volume.Manager
)

func SetReadyChannel(ch chan<- *Service) {
	readyMu.Lock()
	defer readyMu.Unlock()
	readyCh = ch
}

func SetVolumeManager(manager *volume.Manager) {
	readyMu.Lock()
	defer readyMu.Unlock()
	volumeManager = manager
}

func currentVolumeManager() *volume.Manager {
	readyMu.Lock()
	defer readyMu.Unlock()
	return volumeManager
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
			conchplugins.TemplateServicePluginType,
			conchplugins.ImageServicePluginType,
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
			templateInst, err := ic.GetByID(conchplugins.TemplateServicePluginType, conchplugins.TemplateServiceID)
			if err != nil {
				return nil, err
			}
			templates, ok := templateInst.(conchsandbox.TemplateReader)
			if !ok {
				return nil, fmt.Errorf("%s does not provide template reads", conchplugins.TemplateServiceURI)
			}
			imageInst, err := ic.GetByID(conchplugins.ImageServicePluginType, conchplugins.ImageServiceID)
			if err != nil {
				return nil, err
			}
			resolver, ok := imageInst.(conchsandbox.BootResolver)
			if !ok {
				return nil, fmt.Errorf("%s does not provide boot resolution", conchplugins.ImageServiceURI)
			}
			snapshotInst, err := ic.GetByID(conchplugins.SnapshotServicePluginType, conchplugins.SnapshotServiceID)
			if err != nil {
				return nil, err
			}
			snapshots, ok := snapshotInst.(conchsandbox.SnapshotBackend)
			if !ok {
				return nil, fmt.Errorf("%s does not provide snapshot boot layouts", conchplugins.SnapshotServiceURI)
			}
			svc, err := New(
				ic.Context,
				provider.DaemonClient(),
				templates,
				snapshots,
				resolver,
				*cfg,
			)
			if err != nil {
				return nil, err
			}
			svc.manager.SetVolumeManager(currentVolumeManager())
			publishReady(svc)
			return svc, nil
		},
	})
}
