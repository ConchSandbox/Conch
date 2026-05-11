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
}

type Host struct {
	server       *containerdserver.Server
	client       *daemon.Client
	imageService *imageSvc.Service
	cancel       context.CancelFunc
	once         sync.Once
}

func (h *Host) Client() *daemon.Client {
	return h.client
}

func (h *Host) ImageService() *imageSvc.Service {
	return h.imageService
}

func (h *Host) Close() error {
	if h == nil {
		return nil
	}
	var err error
	h.once.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		if h.server != nil {
			h.server.Stop()
		}
		if h.client != nil {
			err = h.client.Close()
		}
	})
	return err
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
	setBootstrapChannel(ready)
	imageSvc.SetReadyChannel(imageReady)
	defer setBootstrapChannel(nil)
	defer imageSvc.SetReadyChannel(nil)
	hostCtx, cancel := context.WithCancel(ctx)

	serverCfg := &serverconfig.Config{
		Version: version.ConfigVersion,
		Root:    filepath.Clean(cfg.RootDir),
		State:   filepath.Clean(cfg.StateDir),
		TempDir: filepath.Join(filepath.Clean(cfg.StateDir), "tmp"),
		RequiredPlugins: []string{
			PluginURI,
			conchplugins.ImageServiceURI,
		},
		Plugins: map[string]any{
			PluginURI: map[string]any{
				"default_namespace": cfg.DefaultNamespace,
			},
		},
	}

	srv, err := containerdserver.New(hostCtx, serverCfg)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start containerd plugin graph: %w", err)
	}

	var (
		inst    *bootstrapInstance
		image   *imageSvc.Service
		timeout = time.After(startTimeout)
	)
	for inst == nil || image == nil {
		select {
		case inst = <-ready:
		case image = <-imageReady:
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
		server:       srv,
		client:       inst.client,
		imageService: image,
		cancel:       cancel,
	}, nil
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
