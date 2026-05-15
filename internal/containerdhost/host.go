package containerdhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	containerdserver "github.com/containerd/containerd/v2/cmd/containerd/server"
	serverconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/version"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/openeuler/Conch/internal/conchplugins"
	imageSvc "github.com/openeuler/Conch/internal/conchservices/image"
	sandboxSvc "github.com/openeuler/Conch/internal/conchservices/sandbox"
	snapshotSvc "github.com/openeuler/Conch/internal/conchservices/snapshot"
	"github.com/openeuler/Conch/internal/daemon"
)

const (
	PluginType = conchplugins.HostPluginType
	PluginID   = conchplugins.HostPluginID
	PluginURI  = conchplugins.HostPluginURI

	defaultNamespace = "default"
	startTimeout     = 10 * time.Second
)

type Config struct {
	RootDir          string
	StateDir         string
	DefaultNamespace string
	Snapshot         SnapshotConfig
	Sandbox          *SandboxConfig
}

type SnapshotConfig struct {
	Enabled bool
	WorkDir string
}

type SandboxConfig struct {
	PoolSize           int
	DynamicReservation bool
	BridgeCount        int
	TapIP              string
	TapMask            int
	VsockSignalRetry   time.Duration
	VsockSignalTimeout time.Duration
	RequestTimeout     time.Duration
}

type Host struct {
	server          *containerdserver.Server
	client          *daemon.Client
	imageService    *imageSvc.Service
	snapshotService *snapshotSvc.Service
	sandboxService  *sandboxSvc.Service
	cancel          context.CancelFunc
	once            sync.Once
}

func (h *Host) Client() *daemon.Client {
	return h.client
}

func (h *Host) ImageService() *imageSvc.Service {
	return h.imageService
}

func (h *Host) SnapshotService() *snapshotSvc.Service {
	return h.snapshotService
}

func (h *Host) SandboxService() *sandboxSvc.Service {
	return h.sandboxService
}

func (h *Host) Close() error {
	if h == nil {
		return nil
	}
	var errs []error
	h.once.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		if h.sandboxService != nil {
			errs = append(errs, h.sandboxService.Close())
		}
		if h.snapshotService != nil {
			errs = append(errs, h.snapshotService.Close())
		}
		if h.client != nil {
			errs = append(errs, h.client.Close())
		}
		if h.server != nil {
			h.server.Stop()
		}
	})
	return errors.Join(errs...)
}

func Start(ctx context.Context, cfg Config) (*Host, error) {
	if cfg.RootDir == "" {
		return nil, errors.New("containerd root dir is required")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("containerd state dir is required")
	}
	if cfg.DefaultNamespace == "" {
		cfg.DefaultNamespace = defaultNamespace
	}
	if err := os.MkdirAll(cfg.RootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create containerd root dir: %w", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create containerd state dir: %w", err)
	}

	ready := make(chan *bootstrapInstance, 1)
	imageReady := make(chan *imageSvc.Service, 1)
	snapshotReady := make(chan *snapshotSvc.Service, 1)
	sandboxReady := make(chan *sandboxSvc.Service, 1)
	setBootstrapChannel(ready)
	imageSvc.SetReadyChannel(imageReady)
	snapshotSvc.SetReadyChannel(snapshotReady)
	sandboxSvc.SetReadyChannel(sandboxReady)
	defer setBootstrapChannel(nil)
	defer imageSvc.SetReadyChannel(nil)
	defer snapshotSvc.SetReadyChannel(nil)
	defer sandboxSvc.SetReadyChannel(nil)
	hostCtx, cancel := context.WithCancel(ctx)

	requiredPlugins := []string{
		PluginURI,
		conchplugins.ImageServiceURI,
	}
	var disabledPlugins []string
	if cfg.Snapshot.Enabled {
		requiredPlugins = append(requiredPlugins, conchplugins.SnapshotServiceURI)
	} else {
		disabledPlugins = append(disabledPlugins, conchplugins.SnapshotServiceURI)
	}
	if cfg.Sandbox != nil {
		if !cfg.Snapshot.Enabled {
			cancel()
			return nil, fmt.Errorf("sandbox service requires snapshot service")
		}
		requiredPlugins = append(requiredPlugins, conchplugins.SandboxServiceURI)
	} else {
		disabledPlugins = append(disabledPlugins, conchplugins.SandboxServiceURI)
	}

	pluginConfigs := map[string]any{
		PluginURI: map[string]any{
			"default_namespace": cfg.DefaultNamespace,
		},
		conchplugins.SnapshotServiceURI: map[string]any{
			"enabled":  cfg.Snapshot.Enabled,
			"work_dir": cfg.Snapshot.WorkDir,
		},
	}
	if cfg.Sandbox != nil {
		pluginConfigs[conchplugins.SandboxServiceURI] = sandboxPluginConfig(cfg.Sandbox)
	}
	serverCfg := &serverconfig.Config{
		Version:         version.ConfigVersion,
		Root:            filepath.Clean(cfg.RootDir),
		State:           filepath.Clean(cfg.StateDir),
		TempDir:         filepath.Join(filepath.Clean(cfg.StateDir), "tmp"),
		DisabledPlugins: disabledPlugins,
		RequiredPlugins: requiredPlugins,
		Plugins:         pluginConfigs,
	}

	srv, err := containerdserver.New(hostCtx, serverCfg)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start containerd plugin graph: %w", err)
	}

	var (
		inst     *bootstrapInstance
		image    *imageSvc.Service
		snapshot *snapshotSvc.Service
		sandbox  *sandboxSvc.Service
		timeout  = time.After(startTimeout)
	)
	for inst == nil || image == nil || (cfg.Snapshot.Enabled && snapshot == nil) || (cfg.Sandbox != nil && sandbox == nil) {
		select {
		case inst = <-ready:
		case image = <-imageReady:
		case snapshot = <-snapshotReady:
		case sandbox = <-sandboxReady:
		case <-ctx.Done():
			cancel()
			srv.Stop()
			return nil, ctx.Err()
		case <-timeout:
			cancel()
			srv.Stop()
			return nil, fmt.Errorf("containerd host required plugins did not initialize")
		}
	}
	return &Host{
		server:          srv,
		client:          inst.client,
		imageService:    image,
		snapshotService: snapshot,
		sandboxService:  sandbox,
		cancel:          cancel,
	}, nil
}

func sandboxPluginConfig(cfg *SandboxConfig) map[string]any {
	if cfg == nil {
		return nil
	}
	return map[string]any{
		"pool_size":            cfg.PoolSize,
		"dynamic_reservation":  cfg.DynamicReservation,
		"bridge_count":         cfg.BridgeCount,
		"tap_ip":               cfg.TapIP,
		"tap_mask":             cfg.TapMask,
		"vsock_signal_retry":   cfg.VsockSignalRetry.String(),
		"vsock_signal_timeout": cfg.VsockSignalTimeout.String(),
		"request_timeout":      cfg.RequestTimeout.String(),
	}
}

type bootstrapConfig struct {
	DefaultNamespace string `toml:"default_namespace" json:"defaultNamespace"`
}

type bootstrapInstance struct {
	client *daemon.Client
}

func (b *bootstrapInstance) DaemonClient() *daemon.Client {
	return b.client
}

var (
	bootstrapMu sync.Mutex
	bootstrapCh chan<- *bootstrapInstance
)

func setBootstrapChannel(ch chan<- *bootstrapInstance) {
	bootstrapMu.Lock()
	defer bootstrapMu.Unlock()
	bootstrapCh = ch
}

func publishBootstrapInstance(inst *bootstrapInstance) {
	bootstrapMu.Lock()
	ch := bootstrapCh
	bootstrapMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- inst:
	default:
	}
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   PluginType,
		ID:     PluginID,
		Config: &bootstrapConfig{},
		Requires: []plugin.Type{
			plugins.EventPlugin,
			plugins.LeasePlugin,
			plugins.SandboxStorePlugin,
			plugins.TransferPlugin,
			plugins.MountManagerPlugin,
			plugins.ServicePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			cfg := ic.Config.(*bootstrapConfig)
			ns := cfg.DefaultNamespace
			if ns == "" {
				ns = defaultNamespace
			}
			client, err := daemon.NewInMemory(ic, containerd.WithDefaultNamespace(ns))
			if err != nil {
				return nil, err
			}
			inst := &bootstrapInstance{client: client}
			publishBootstrapInstance(inst)
			return inst, nil
		},
	})
}
