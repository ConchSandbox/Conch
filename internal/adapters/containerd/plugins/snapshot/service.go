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

func ensureSnapshotCanRemoveAlone(ctx context.Context, snapshotter snapshots.Snapshotter, key string) error {
	info, err := snapshotter.Stat(ctx, key)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("stat snapshot %s: %w", key, err)
	}
	if labelsConchSnapshotGroup(info.Labels) {
		return fmt.Errorf("snapshot %s is a Conch snapshot group root; use cascade remove", key)
	}
	rootfs, err := findReferencingRootfs(ctx, snapshotter, key)
	if err != nil {
		return err
	}
	if rootfs != "" {
		return fmt.Errorf("snapshot %s is referenced by Conch rootfs snapshot %s; use cascade remove", key, rootfs)
	}
	return nil
}

func removeSnapshotCascade(ctx context.Context, snapshotter snapshots.Snapshotter, key string) error {
	group, err := resolveSnapshotGroup(ctx, snapshotter, key)
	if err != nil {
		return err
	}
	if err := preserveSnapshotGroupLabels(ctx, snapshotter, group); err != nil {
		return err
	}
	var removeErr error
	for _, removeKey := range group.removeOrder() {
		if err := removeSnapshotKey(ctx, snapshotter, removeKey); err != nil {
			err = fmt.Errorf("remove snapshot %s: %w", removeKey, err)
			if removeKey == group.Rootfs {
				return err
			}
			removeErr = errors.Join(removeErr, err)
		}
	}
	return removeErr
}

func preserveSnapshotGroupLabels(ctx context.Context, snapshotter snapshots.Snapshotter, group snapshotGroup) error {
	labels := group.labels()
	if len(labels) == 0 {
		return nil
	}
	for _, key := range group.componentKeys() {
		info, err := snapshotter.Stat(ctx, key)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("stat snapshot %s before cascade remove: %w", key, err)
		}
		if info.Labels == nil {
			info.Labels = map[string]string{}
		}
		var fieldpaths []string
		for label, value := range labels {
			if value == "" || info.Labels[label] == value {
				continue
			}
			info.Labels[label] = value
			fieldpaths = append(fieldpaths, "labels."+label)
		}
		if len(fieldpaths) == 0 {
			continue
		}
		if _, err := snapshotter.Update(ctx, info, fieldpaths...); err != nil {
			return fmt.Errorf("preserve snapshot group labels on %s: %w", key, err)
		}
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

func (g snapshotGroup) labels() map[string]string {
	labels := map[string]string{}
	if strings.TrimSpace(g.Rootfs) != "" {
		labels[common.SnapshotLabelRootfsSnapshot] = strings.TrimSpace(g.Rootfs)
	}
	if strings.TrimSpace(g.Mem) != "" {
		labels[common.SnapshotLabelMemSnapshot] = strings.TrimSpace(g.Mem)
	}
	if strings.TrimSpace(g.VM) != "" {
		labels[common.SnapshotLabelVMSnapshot] = strings.TrimSpace(g.VM)
	}
	return labels
}

func (g snapshotGroup) componentKeys() []string {
	out := []string{}
	for _, key := range []string{g.Mem, g.VM} {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if !containsString(out, key) {
			out = append(out, key)
		}
	}
	return out
}

func (g snapshotGroup) removeOrder() []string {
	out := []string{}
	for _, key := range []string{g.Rootfs, g.Mem, g.VM} {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if !containsString(out, key) {
			out = append(out, key)
		}
	}
	return out
}

func resolveSnapshotGroup(ctx context.Context, snapshotter snapshots.Snapshotter, key string) (snapshotGroup, error) {
	info, err := snapshotter.Stat(ctx, key)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return snapshotGroup{}, nil
		}
		return snapshotGroup{}, fmt.Errorf("stat snapshot %s: %w", key, err)
	}
	if labelsConchSnapshotGroup(info.Labels) {
		return groupFromRootfsInfo(info), nil
	}
	rootfs, err := findReferencingRootfs(ctx, snapshotter, key)
	if err != nil {
		return snapshotGroup{}, err
	}
	if rootfs == "" {
		return snapshotGroup{Rootfs: key}, nil
	}
	rootfsInfo, err := snapshotter.Stat(ctx, rootfs)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return snapshotGroup{Rootfs: key}, nil
		}
		return snapshotGroup{}, fmt.Errorf("stat referencing rootfs snapshot %s: %w", rootfs, err)
	}
	return groupFromRootfsInfo(rootfsInfo), nil
}

func groupFromRootfsInfo(info snapshots.Info) snapshotGroup {
	rootfs := strings.TrimSpace(info.Labels[common.SnapshotLabelRootfsSnapshot])
	if rootfs == "" {
		rootfs = info.Name
	}
	return snapshotGroup{
		Rootfs: rootfs,
		Mem:    strings.TrimSpace(info.Labels[common.SnapshotLabelMemSnapshot]),
		VM:     strings.TrimSpace(info.Labels[common.SnapshotLabelVMSnapshot]),
	}
}

func labelsConchSnapshotGroup(labels map[string]string) bool {
	if len(labels) == 0 {
		return false
	}
	return strings.TrimSpace(labels[common.SnapshotLabelMemSnapshot]) != "" ||
		strings.TrimSpace(labels[common.SnapshotLabelVMSnapshot]) != ""
}

func findReferencingRootfs(ctx context.Context, snapshotter snapshots.Snapshotter, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	var refs []string
	if err := snapshotter.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		if info.Labels[common.SnapshotLabelMemSnapshot] == key ||
			info.Labels[common.SnapshotLabelVMSnapshot] == key {
			refs = append(refs, info.Name)
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk snapshots: %w", err)
	}
	sort.Strings(refs)
	if len(refs) == 0 {
		return "", nil
	}
	if len(refs) > 1 {
		return "", errors.New("snapshot is referenced by multiple Conch rootfs snapshots")
	}
	return refs[0], nil
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
	return Meta{
		Key:       info.Name,
		Kind:      strings.ToLower(info.Kind.String()),
		Parent:    info.Parent,
		Labels:    info.Labels,
		CreatedAt: info.Created,
		UpdatedAt: info.Updated,
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
