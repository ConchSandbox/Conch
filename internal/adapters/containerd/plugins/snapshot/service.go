package snapshot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/conchplugins"
	conchsnapshot "github.com/openeuler/Conch/internal/snapshot"
)

const SnapshotLabelVMSnapshot = "conch/snapshotter/vm-snapshot"

type Config struct {
	Enabled bool   `toml:"enabled" json:"enabled"`
	WorkDir string `toml:"work_dir" json:"workDir"`
}

type LinkVMRequest struct {
	RootfsSnapshotID string
	VMSnapshotID     string
	Namespace        string
}

type InfoRequest struct {
	Key       string
	Namespace string
}

type Meta struct {
	Key         string            `json:"key"`
	Parent      string            `json:"parent,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	StoragePath string            `json:"storage_path,omitempty"`
}

type Chain struct {
	Info       Meta     `json:"info"`
	ChainPaths []string `json:"chain_paths"`
}

type Service struct {
	client *containerdclient.Client
}

func New(client *containerdclient.Client, workDir string) (*Service, error) {
	if workDir == "" {
		return nil, fmt.Errorf("snapshot work dir is required")
	}
	if err := conchsnapshot.NewServer(workDir, client); err != nil {
		return nil, err
	}
	return &Service{client: client}, nil
}

func (s *Service) Close() error {
	return conchsnapshot.Close()
}

func (s *Service) LinkVM(ctx context.Context, req LinkVMRequest) error {
	if req.RootfsSnapshotID == "" || req.VMSnapshotID == "" {
		return fmt.Errorf("rootfs and sandbox snapshot IDs are required")
	}
	snapshotter := s.client.SnapshotService("erofs")
	snapshotCtx, err := snapshotNamespaceContext(ctx, s.client, req.Namespace)
	if err != nil {
		return err
	}
	_, err = snapshotter.Update(snapshotCtx, snapshots.Info{
		Name: req.RootfsSnapshotID,
		Labels: map[string]string{
			SnapshotLabelVMSnapshot: req.VMSnapshotID,
		},
	}, "labels."+SnapshotLabelVMSnapshot)
	return err
}

func (s *Service) Info(ctx context.Context, req InfoRequest) (Meta, error) {
	if req.Key == "" {
		return Meta{}, fmt.Errorf("key is required")
	}
	snapshotter := s.client.SnapshotService("erofs")
	snapshotCtx, err := snapshotNamespaceContext(ctx, s.client, req.Namespace)
	if err != nil {
		return Meta{}, err
	}

	stat, err := snapshotter.Stat(snapshotCtx, req.Key)
	if err != nil {
		return Meta{}, fmt.Errorf("stat failed for key %s: %w", req.Key, err)
	}

	mounts, err := snapshotter.Mounts(snapshotCtx, req.Key)
	if err != nil || len(mounts) == 0 || len(mounts[0].Options) == 0 {
		viewID := fmt.Sprintf("tmp-v-%d-%s", time.Now().UnixNano(), req.Key)
		mounts, err = snapshotter.View(snapshotCtx, viewID, req.Key)
		if err != nil {
			return Meta{}, fmt.Errorf("failed to resolve storage path via mounts or view: %w", err)
		}
		defer snapshotter.Remove(snapshotCtx, viewID)
	}

	storagePath := ""
	if len(mounts) > 0 {
		for _, opt := range mounts[0].Options {
			if strings.HasPrefix(opt, "upperdir=") {
				storagePath = strings.TrimPrefix(opt, "upperdir=")
				break
			}
			if strings.HasPrefix(opt, "lowerdir=") && storagePath == "" {
				storagePath = strings.Split(strings.TrimPrefix(opt, "lowerdir="), ":")[0]
			}
		}
		if storagePath == "" || storagePath == "overlay" {
			storagePath = mounts[0].Source
		}
	}

	return Meta{
		Key:         stat.Name,
		Parent:      stat.Parent,
		Labels:      stat.Labels,
		StoragePath: storagePath,
	}, nil
}

func (s *Service) Chain(ctx context.Context, req InfoRequest) (Chain, error) {
	var rev []string
	var first Meta
	cur := req.Key
	for cur != "" {
		info, err := s.Info(ctx, InfoRequest{Key: cur, Namespace: req.Namespace})
		if err != nil {
			return Chain{}, fmt.Errorf("snapshot %s: %w", cur, err)
		}
		if first.Key == "" {
			first = info
		}
		if info.StoragePath == "" {
			return Chain{}, fmt.Errorf("snapshot %s has empty storage path", cur)
		}
		rev = append(rev, info.StoragePath)
		cur = info.Parent
	}

	out := make([]string, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return Chain{Info: first, ChainPaths: out}, nil
}

func snapshotNamespaceContext(ctx context.Context, client *containerdclient.Client, namespace string) (context.Context, error) {
	ns := namespace
	if ns == "" && client != nil {
		ns = client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	if client != nil {
		return client.WithNamespace(ctx, ns)
	}
	return namespaces.WithNamespace(ctx, ns), nil
}

var (
	readyMu sync.Mutex
	readyCh chan<- *Service
)

func SetReadyChannel(ch chan<- *Service) {
	readyMu.Lock()
	defer readyMu.Unlock()
	readyCh = ch
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
		Type:   conchplugins.SnapshotServicePluginType,
		ID:     conchplugins.SnapshotServiceID,
		Config: &Config{},
		Requires: []plugin.Type{
			conchplugins.HostPluginType,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			cfg := ic.Config.(*Config)
			if !cfg.Enabled {
				return nil, plugin.ErrSkipPlugin
			}
			inst, err := ic.GetByID(conchplugins.HostPluginType, conchplugins.HostPluginID)
			if err != nil {
				return nil, err
			}
			provider, ok := inst.(daemonClientProvider)
			if !ok {
				return nil, fmt.Errorf("%s does not provide daemon client", conchplugins.HostPluginURI)
			}
			svc, err := New(provider.DaemonClient(), cfg.WorkDir)
			if err != nil {
				return nil, err
			}
			publishReady(svc)
			return svc, nil
		},
	})
}
