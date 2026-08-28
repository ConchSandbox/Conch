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
	containerdtemplate "github.com/openeuler/Conch/internal/adapters/containerd/template"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	conchsandbox "github.com/openeuler/Conch/internal/sandbox"
	conchsnapshot "github.com/openeuler/Conch/internal/snapshot"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

const (
	pluginType plugin.Type = "io.conch.internal.v1"
	pluginID               = "containerd-host"
	pluginURI              = string(pluginType) + "." + pluginID

	startTimeout = 10 * time.Second
)

type Config struct {
	RootDir  string
	StateDir string
	Snapshot SnapshotConfig
	Sandbox  *conchsandbox.Config
}

type SnapshotConfig struct {
	WorkDir string
}

type Host struct {
	server         *containerdserver.Server
	client         *containerdclient.Client
	snapshotServer *conchsnapshot.Server
	templateStore  conchtemplate.Store
	sandboxManager *conchsandbox.Manager
	cancel         context.CancelFunc
	once           sync.Once
}

func (h *Host) Client() *containerdclient.Client {
	return h.client
}

func (h *Host) SnapshotServer() *conchsnapshot.Server {
	return h.snapshotServer
}

func (h *Host) TemplateStore() conchtemplate.Store {
	return h.templateStore
}

func (h *Host) SandboxManager() *conchsandbox.Manager {
	return h.sandboxManager
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
		if h.sandboxManager != nil {
			finish := cleanupdiag.Start("containerd_host.sandbox_manager.close")
			err := h.sandboxManager.Close()
			finish(err)
			errs = append(errs, err)
		}
		if h.snapshotServer != nil {
			finish := cleanupdiag.Start("containerd_host.snapshot_server.close")
			err := h.snapshotServer.Close()
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
	if err := os.MkdirAll(cfg.RootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create containerd root dir: %w", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create containerd state dir: %w", err)
	}

	hostCtx, cancel := context.WithCancel(ctx)

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
	}
	serverCfg := &serverconfig.Config{
		Version:         version.ConfigVersion,
		Root:            filepath.Clean(cfg.RootDir),
		State:           filepath.Clean(cfg.StateDir),
		TempDir:         filepath.Join(filepath.Clean(cfg.StateDir), "tmp"),
		RequiredPlugins: []string{pluginURI},
		Plugins:         pluginConfigs,
	}

	srv, inst, err := startContainerdPluginGraph(hostCtx, serverCfg)
	if err != nil {
		cancel()
		return nil, err
	}

	host := &Host{
		server: srv,
		client: inst.client,
		cancel: cancel,
	}
	fail := func(component string, initErr error) (*Host, error) {
		cleanupErr := host.Close()
		return nil, errors.Join(
			fmt.Errorf("initialize %s: %w", component, initErr),
			cleanupErr,
		)
	}

	host.snapshotServer, err = conchsnapshot.NewServer(cfg.Snapshot.WorkDir, inst.client)
	if err != nil {
		return fail("snapshot server", err)
	}

	host.templateStore = containerdtemplate.NewStore(inst.client)

	if cfg.Sandbox != nil {
		host.sandboxManager, err = conchsandbox.New(
			hostCtx,
			inst.client,
			host.snapshotServer,
			*cfg.Sandbox,
		)
		if err != nil {
			return fail("sandbox manager", err)
		}
	}

	return host, nil
}

func startContainerdPluginGraph(ctx context.Context, cfg *serverconfig.Config) (*containerdserver.Server, *bootstrapInstance, error) {
	bootstrapStartMu.Lock()
	defer bootstrapStartMu.Unlock()

	ready := make(chan *bootstrapInstance, 1)
	setBootstrapChannel(ready)
	defer setBootstrapChannel(nil)

	srv, err := containerdserver.New(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("start containerd plugin graph: %w", err)
	}

	timer := time.NewTimer(startTimeout)
	defer timer.Stop()
	select {
	case inst := <-ready:
		return srv, inst, nil
	case <-ctx.Done():
		srv.Stop()
		return nil, nil, ctx.Err()
	case <-timer.C:
		srv.Stop()
		return nil, nil, fmt.Errorf("containerd host bootstrap plugin did not initialize")
	}
}

type bootstrapConfig struct {
}

type bootstrapInstance struct {
	client *containerdclient.Client
}

var (
	bootstrapStartMu sync.Mutex
	bootstrapMu      sync.Mutex
	bootstrapCh      chan<- *bootstrapInstance
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
		Type:   pluginType,
		ID:     pluginID,
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
