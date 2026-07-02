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
	"github.com/openeuler/Conch/internal/snapshot/common"
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
	Cascade   bool   `json:"cascade,omitempty"`
}

type Meta struct {
	Key         string            `json:"key"`
	Kind        string            `json:"kind,omitempty"`
	ConchRole   string            `json:"conch_role,omitempty"`
	GroupID     string            `json:"group_id,omitempty"`
	Parent      string            `json:"parent,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	StoragePath string            `json:"storage_path,omitempty"`
	CreatedAt   time.Time         `json:"created_at,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at,omitempty"`
}

type Chain struct {
	Info       Meta     `json:"info"`
	ChainPaths []string `json:"chain_paths"`
}

const (
	conchRoleStandalone = "standalone"
	conchRoleRootfs     = "rootfs"
	conchRoleMem        = "mem"
	conchRoleVM         = "vm"
	conchRoleUnknown    = "unknown"
)

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
	finishClose := cleanupdiag.Start("snapshot_service.close")
	err := conchsnapshot.Close()
	finishClose(err)
	return err
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
	if req.Cascade {
		return removeSnapshotCascade(snapshotCtx, snapshotter, req.Key)
	}
	if err := ensureSnapshotCanRemoveAlone(snapshotCtx, snapshotter, req.Key); err != nil {
		return err
	}
	if err := removeSnapshotKey(snapshotCtx, snapshotter, req.Key); err != nil {
		return fmt.Errorf("remove snapshot %s: %w", req.Key, err)
	}
	return nil
}

func removeSnapshotCascade(ctx context.Context, snapshotter snapshots.Snapshotter, key string) error {
	info, err := snapshotter.Stat(ctx, key)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("stat snapshot %s: %w", key, err)
	}
	if componentRootfs := snapshotComponentRootfs(info); componentRootfs != "" {
		return fmt.Errorf("snapshot %s is a Conch snapshot component of rootfs %s; cascade remove requires the group rootfs snapshot key", key, componentRootfs)
	}

	group := snapshotGroup{Rootfs: key}
	if labelsConchSnapshotGroup(info.Labels) {
		group = groupFromRootfsInfo(info)
	}

	if err := removeSnapshotKey(ctx, snapshotter, group.Rootfs); err != nil {
		return fmt.Errorf("remove snapshot %s: %w", group.Rootfs, err)
	}
	var removeErr error
	for _, removeKey := range group.componentRemoveOrder() {
		if err := removeSnapshotKey(ctx, snapshotter, removeKey); err != nil {
			removeErr = errors.Join(removeErr, fmt.Errorf("remove snapshot %s: %w", removeKey, err))
		}
	}
	if removeErr != nil {
		return removeErr
	}
	return nil
}

func ensureSnapshotCanRemoveAlone(ctx context.Context, snapshotter snapshots.Snapshotter, key string) error {
	info, err := snapshotter.Stat(ctx, key)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("stat snapshot %s: %w", key, err)
	}
	componentRootfs := snapshotComponentRootfs(info)
	if componentRootfs != "" {
		return fmt.Errorf("snapshot %s is a Conch snapshot component of rootfs %s; use --cascade on the group rootfs snapshot", key, componentRootfs)
	}
	if labelsConchSnapshotGroup(info.Labels) {
		return fmt.Errorf("snapshot %s is the rootfs anchor of a Conch snapshot group; use --cascade to remove the group", key)
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

type snapshotGroup struct {
	Rootfs string
	Mem    string
	VM     string
}

func (g snapshotGroup) componentRemoveOrder() []string {
	out := []string{}
	for _, key := range []string{g.Mem, g.VM} {
		if strings.TrimSpace(key) == "" || key == g.Rootfs {
			continue
		}
		if !containsString(out, key) {
			out = append(out, key)
		}
	}
	return out
}

func groupFromRootfsInfo(info snapshots.Info) snapshotGroup {
	rootfs := strings.TrimSpace(info.Labels[common.SnapshotLabelGroupID])
	if rootfs == "" {
		rootfs = info.Name
	}
	return snapshotGroup{
		Rootfs: rootfs,
		Mem:    strings.TrimSpace(info.Labels[common.SnapshotLabelGroupMemRef]),
		VM:     strings.TrimSpace(info.Labels[common.SnapshotLabelGroupVMRef]),
	}
}

func labelsConchSnapshotGroup(labels map[string]string) bool {
	if len(labels) == 0 {
		return false
	}
	return strings.TrimSpace(labels[common.SnapshotLabelGroupMemRef]) != "" ||
		strings.TrimSpace(labels[common.SnapshotLabelGroupVMRef]) != ""
}

func snapshotComponentRootfs(info snapshots.Info) string {
	rootfs := strings.TrimSpace(info.Labels[common.SnapshotLabelGroupID])
	if rootfs == "" || rootfs == info.Name {
		return ""
	}
	return rootfs
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
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

func snapshotMeta(info snapshots.Info) Meta {
	meta := Meta{
		Key:       info.Name,
		Kind:      strings.ToLower(info.Kind.String()),
		Parent:    info.Parent,
		Labels:    info.Labels,
		CreatedAt: info.Created,
		UpdatedAt: info.Updated,
	}
	if rootfs := snapshotComponentRootfs(info); rootfs != "" {
		meta.ConchRole = snapshotComponentRole(info)
		meta.GroupID = rootfs
		return meta
	}
	if labelsConchSnapshotGroup(info.Labels) {
		meta.ConchRole = conchRoleRootfs
		meta.GroupID = info.Name
		return meta
	}
	meta.ConchRole = conchRoleStandalone
	return meta
}

func snapshotComponentRole(info snapshots.Info) string {
	switch strings.TrimSpace(info.Labels[common.SnapshotLabelComponentKind]) {
	case common.SnapshotComponentKindMem:
		return conchRoleMem
	case common.SnapshotComponentKindVM:
		return conchRoleVM
	default:
		return conchRoleUnknown
	}
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
