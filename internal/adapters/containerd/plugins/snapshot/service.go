package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/conchplugins"
	conchsnapshot "github.com/openeuler/Conch/internal/snapshot"
)

var ErrInvalidRequest = errors.New("invalid snapshot request")

type Config struct {
	WorkDir string `toml:"work_dir" json:"workDir"`
}

type InfoRequest struct {
	Key       string
	Namespace string
}

type ListRequest struct {
	Namespace string   `json:"namespace,omitempty"`
	Filters   []string `json:"filters,omitempty"`
}

type RemoveRequest struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
}

type Meta struct {
	Key         string            `json:"key"`
	Kind        string            `json:"kind,omitempty"`
	Parent      string            `json:"parent,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	StoragePath string            `json:"storage_path,omitempty"`
	CreatedAt   time.Time         `json:"created_at,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at,omitempty"`
}

type Service struct {
	client *containerdclient.Client
	server *conchsnapshot.Server
}

// ServerProvider exposes the runtime snapshot server held by the snapshot plugin.
type ServerProvider interface {
	SnapshotServer() *conchsnapshot.Server
}

func New(client *containerdclient.Client, workDir string) (*Service, error) {
	if workDir == "" {
		return nil, fmt.Errorf("snapshot work dir is required")
	}
	server, err := conchsnapshot.NewServer(workDir, client)
	if err != nil {
		return nil, err
	}
	return &Service{client: client, server: server}, nil
}

func (s *Service) Close() error {
	finishClose := cleanupdiag.Start("snapshot_service.close")
	var err error
	if s != nil && s.server != nil {
		err = s.server.Close()
	}
	finishClose(err)
	return err
}

func (s *Service) SnapshotServer() *conchsnapshot.Server {
	if s == nil {
		return nil
	}
	return s.server
}

func (s *Service) List(ctx context.Context, req ListRequest) ([]Meta, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("snapshot service has no containerd client")
	}
	snapshotter := s.client.SnapshotService("erofs")
	snapshotCtx, err := snapshotNamespaceContext(ctx, s.client, req.Namespace)
	if err != nil {
		return nil, err
	}
	var out []Meta
	if err := snapshotter.Walk(snapshotCtx, func(_ context.Context, info snapshots.Info) error {
		out = append(out, snapshotMeta(info))
		return nil
	}, req.Filters...); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func (s *Service) Remove(ctx context.Context, req RemoveRequest) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("snapshot service has no containerd client")
	}
	if req.Key == "" {
		return fmt.Errorf("%w: key is required", ErrInvalidRequest)
	}
	snapshotter := s.client.SnapshotService("erofs")
	snapshotCtx, err := snapshotNamespaceContext(ctx, s.client, req.Namespace)
	if err != nil {
		return err
	}
	if err := removeSnapshotKey(snapshotCtx, snapshotter, req.Key); err != nil {
		return fmt.Errorf("remove snapshot %s: %w", req.Key, err)
	}
	return nil
}

func removeSnapshotKey(ctx context.Context, snapshotter snapshots.Snapshotter, key string) error {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	if err := snapshotter.Remove(ctx, key); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
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

	meta := snapshotMeta(stat)
	meta.StoragePath = storagePath
	return meta, nil
}

func snapshotMeta(info snapshots.Info) Meta {
	meta := Meta{
		Key:       info.Name,
		Kind:      strings.ToLower(info.Kind.String()),
		Parent:    info.Parent,
		Labels:    info.Labels,
		CreatedAt: info.Created,
		UpdatedAt: info.Updated,
	}
	return meta
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
