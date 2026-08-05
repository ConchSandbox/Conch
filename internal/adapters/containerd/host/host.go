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
	"github.com/containerd/containerd/v2/plugins/services"
	"github.com/containerd/containerd/v2/version"
	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	imageSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/image"
	sandboxSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/sandbox"
	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	templateSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/template"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/conchplugins"
	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/volume"
)

const (
	PluginType = conchplugins.HostPluginType
	PluginID   = conchplugins.HostPluginID
	PluginURI  = conchplugins.HostPluginURI

	startTimeout = 10 * time.Second
)

type Config struct {
	RootDir       string
	StateDir      string
	Snapshot      SnapshotConfig
	TemplateStore state.Store
	Sandbox       *SandboxConfig
}

type SnapshotConfig struct {
	WorkDir string
}

type SandboxConfig struct {
	WarmPoolSize       int
	TapIP              string
	TapMask            int
	CNI                netstack.CNIManagerConfig
	VsockSignalRetry   time.Duration
	VsockSignalTimeout time.Duration
	RequestTimeout     time.Duration
	VolumeManager      *volume.Manager
}

type Host struct {
	server          *containerdserver.Server
	client          *containerdclient.Client
	imageService    *imageSvc.Service
	snapshotService *snapshotSvc.Service
	templateService *templateSvc.Service
	sandboxService  *sandboxSvc.Service
	cancel          context.CancelFunc
	once            sync.Once
}

func (h *Host) Client() *containerdclient.Client {
	return h.client
}

func (h *Host) ImageService() *imageSvc.Service {
	return h.imageService
}

func (h *Host) SnapshotService() *snapshotSvc.Service {
	return h.snapshotService
}

func (h *Host) TemplateService() *templateSvc.Service {
	return h.templateService
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
		finishHost := cleanupdiag.Start("containerd_host.close")
		defer func() {
			finishHost(errors.Join(errs...))
		}()

		if h.cancel != nil {
			finish := cleanupdiag.Start("containerd_host.cancel")
			h.cancel()
			finish(nil)
		}
		if h.sandboxService != nil {
			finish := cleanupdiag.Start("containerd_host.sandbox_service.close")
			err := h.sandboxService.Close()
			finish(err)
			errs = append(errs, err)
		}
		if h.snapshotService != nil {
			finish := cleanupdiag.Start("containerd_host.snapshot_service.close")
			err := h.snapshotService.Close()
			finish(err)
			errs = append(errs, err)
		}
		if h.client != nil {
			finish := cleanupdiag.Start("containerd_host.client.close")
			err := h.client.Close()
			finish(err)
			errs = append(errs, err)
		}
		if h.server != nil {
			finish := cleanupdiag.Start("containerd_host.server.stop")
			h.server.Stop()
			finish(nil)
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
	if cfg.Sandbox != nil && cfg.TemplateStore == nil {
		return nil, errors.New("template store is required when sandbox service is enabled")
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
	templateReady := make(chan *templateSvc.Service, 1)
	sandboxReady := make(chan *sandboxSvc.Service, 1)
	setBootstrapChannel(ready)
	imageSvc.SetReadyChannel(imageReady)
	snapshotSvc.SetReadyChannel(snapshotReady)
	templateSvc.SetReadyChannel(templateReady)
	sandboxSvc.SetReadyChannel(sandboxReady)
	defer setBootstrapChannel(nil)
	defer imageSvc.SetReadyChannel(nil)
	defer snapshotSvc.SetReadyChannel(nil)
	defer templateSvc.SetReadyChannel(nil)
	defer sandboxSvc.SetReadyChannel(nil)
	if cfg.TemplateStore != nil {
		templateSvc.SetStateStore(cfg.TemplateStore)
		defer templateSvc.SetStateStore(nil)
	}
	if cfg.Sandbox != nil {
		sandboxSvc.SetVolumeManager(cfg.Sandbox.VolumeManager)
		defer sandboxSvc.SetVolumeManager(nil)
	}
	hostCtx, cancel := context.WithCancel(ctx)

	requiredPlugins := []string{
		PluginURI,
		conchplugins.ImageServiceURI,
		conchplugins.SnapshotServiceURI,
	}
	var disabledPlugins []string
	if cfg.TemplateStore != nil {
		requiredPlugins = append(requiredPlugins, conchplugins.TemplateServiceURI)
	} else {
		disabledPlugins = append(disabledPlugins, conchplugins.TemplateServiceURI)
	}
	if cfg.Sandbox != nil {
		requiredPlugins = append(requiredPlugins, conchplugins.SandboxServiceURI)
	} else {
		disabledPlugins = append(disabledPlugins, conchplugins.SandboxServiceURI)
	}

	pluginConfigs := map[string]any{
		string(plugins.ServicePlugin) + "." + services.DiffService: map[string]any{
			"default": []string{"erofs", "walking"},
		},
		string(plugins.TransferPlugin) + ".local": map[string]any{
			"unpack_config": []map[string]any{
				{
					"platform":    platforms.Format(platforms.DefaultSpec()),
					"snapshotter": "erofs",
					"differ":      "erofs",
				},
			},
		},
		string(plugins.DiffPlugin) + ".erofs": map[string]any{
			"mkfs_options": []string{"--fsalignblks=512"},
		},
		conchplugins.SnapshotServiceURI: map[string]any{
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
		template *templateSvc.Service
		sandbox  *sandboxSvc.Service
		timeout  = time.After(startTimeout)
	)
	for inst == nil || image == nil || snapshot == nil || (cfg.TemplateStore != nil && template == nil) || (cfg.Sandbox != nil && sandbox == nil) {
		select {
		case inst = <-ready:
		case image = <-imageReady:
		case snapshot = <-snapshotReady:
		case template = <-templateReady:
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
		templateService: template,
		sandboxService:  sandbox,
		cancel:          cancel,
	}, nil
}

func sandboxPluginConfig(cfg *SandboxConfig) map[string]any {
	if cfg == nil {
		return nil
	}
	return map[string]any{
		"warm_pool_size":       cfg.WarmPoolSize,
		"tap_ip":               cfg.TapIP,
		"tap_mask":             cfg.TapMask,
		"cni":                  cfg.CNI,
		"vsock_signal_retry":   cfg.VsockSignalRetry.String(),
		"vsock_signal_timeout": cfg.VsockSignalTimeout.String(),
		"request_timeout":      cfg.RequestTimeout.String(),
	}
}

type bootstrapConfig struct {
}

type bootstrapInstance struct {
	client *containerdclient.Client
}

func (b *bootstrapInstance) DaemonClient() *containerdclient.Client {
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
			client, err := containerdclient.NewInMemory(ic, containerd.WithDefaultNamespace(containerdclient.Namespace))
			if err != nil {
				return nil, err
			}
			inst := &bootstrapInstance{client: client}
			publishBootstrapInstance(inst)
			return inst, nil
		},
	})
}
