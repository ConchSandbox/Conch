package snapshot

import (
	"context"
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
	"github.com/openeuler/Conch/internal/runtimeapi"
	conchsnapshot "github.com/openeuler/Conch/internal/snapshot"
)

type Config struct {
	WorkDir string `toml:"work_dir" json:"workDir"`
}

type Service struct {
	client *containerdclient.Client
	server *conchsnapshot.Server
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

func (s *Service) CreateBootLayout(
	ctx context.Context,
	namespace string,
	key string,
	req conchsnapshot.BootLayoutRequest,
) (*conchsnapshot.BootLayout, error) {
	if s == nil || s.server == nil {
		return nil, fmt.Errorf("snapshot service is not configured")
	}
	return s.server.CreateBootLayout(ctx, namespace, key, req)
}

func (s *Service) RestoreBootLayout(
	ctx context.Context,
	namespace string,
	key string,
	req conchsnapshot.BootLayoutRequest,
) (*conchsnapshot.BootLayout, error) {
	if s == nil || s.server == nil {
		return nil, fmt.Errorf("snapshot service is not configured")
	}
	return s.server.RestoreBootLayout(ctx, namespace, key, req)
}

func (s *Service) ReleaseBootLayout(ctx context.Context, namespace, key string) error {
	if s == nil || s.server == nil {
		return fmt.Errorf("snapshot service is not configured")
	}
	return s.server.ReleaseBootLayout(ctx, namespace, key)
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

func (s *Service) List(ctx context.Context, opts runtimeapi.ListSnapshotsOptions) ([]runtimeapi.SnapshotRecord, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("snapshot service has no containerd client")
	}
	snapshotter := s.client.SnapshotService("erofs")
	snapshotCtx, err := snapshotNamespaceContext(ctx, s.client, opts.Namespace)
	if err != nil {
		return nil, err
	}
	var out []runtimeapi.SnapshotRecord
	if err := snapshotter.Walk(snapshotCtx, func(_ context.Context, info snapshots.Info) error {
		out = append(out, snapshotRecord(info))
		return nil
	}, opts.Filters...); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func (s *Service) Remove(ctx context.Context, opts runtimeapi.RemoveSnapshotOptions) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("snapshot service has no containerd client")
	}
	if opts.Key == "" {
		return fmt.Errorf("key is required")
	}
	snapshotter := s.client.SnapshotService("erofs")
	snapshotCtx, err := snapshotNamespaceContext(ctx, s.client, opts.Namespace)
	if err != nil {
		return err
	}
	if err := removeSnapshotKey(snapshotCtx, snapshotter, opts.Key); err != nil {
		return fmt.Errorf("remove snapshot %s: %w", opts.Key, err)
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

func (s *Service) Info(ctx context.Context, opts runtimeapi.SnapshotInfoOptions) (runtimeapi.SnapshotRecord, error) {
	if opts.Key == "" {
		return runtimeapi.SnapshotRecord{}, fmt.Errorf("key is required")
	}
	snapshotter := s.client.SnapshotService("erofs")
	snapshotCtx, err := snapshotNamespaceContext(ctx, s.client, opts.Namespace)
	if err != nil {
		return runtimeapi.SnapshotRecord{}, err
	}

	stat, err := snapshotter.Stat(snapshotCtx, opts.Key)
	if err != nil {
		return runtimeapi.SnapshotRecord{}, fmt.Errorf("stat failed for key %s: %w", opts.Key, err)
	}

	mounts, err := snapshotter.Mounts(snapshotCtx, opts.Key)
	if err != nil || len(mounts) == 0 || len(mounts[0].Options) == 0 {
		viewID := fmt.Sprintf("tmp-v-%d-%s", time.Now().UnixNano(), opts.Key)
		mounts, err = snapshotter.View(snapshotCtx, viewID, opts.Key)
		if err != nil {
			return runtimeapi.SnapshotRecord{}, fmt.Errorf("failed to resolve storage path via mounts or view: %w", err)
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

	record := snapshotRecord(stat)
	record.StoragePath = storagePath
	return record, nil
}

func snapshotRecord(info snapshots.Info) runtimeapi.SnapshotRecord {
	record := runtimeapi.SnapshotRecord{
		Key:       info.Name,
		Kind:      strings.ToLower(info.Kind.String()),
		Parent:    info.Parent,
		Labels:    info.Labels,
		CreatedAt: info.Created,
		UpdatedAt: info.Updated,
	}
	return record
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
