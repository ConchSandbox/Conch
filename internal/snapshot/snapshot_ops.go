package snapshot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

// snapshotOps provides helper operations for snapshot management.
type snapshotOps struct {
	server *server
}

func mountActivationKey(prefix, namespace, key string) string {
	value := prefix + "-" + namespace + "-" + key
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "@", "_")
	return replacer.Replace(value)
}

func activateAndMount(ctx context.Context, mgr mount.Manager, namespace, activationKey string, mounts []mount.Mount, mountPoint string) (string, error) {
	if len(mounts) == 0 {
		return "", fmt.Errorf("snapshot returned no mount info")
	}
	activated := false
	if namespace != "" {
		ctx = namespaces.WithNamespace(ctx, namespace)
	}
	if mgr != nil {
		info, err := mgr.Activate(ctx, activationKey, mounts)
		if err == nil {
			mounts = info.System
			if len(mounts) == 0 && len(info.Active) > 0 {
				mounts = []mount.Mount{{
					Type:    "bind",
					Source:  info.Active[len(info.Active)-1].MountPoint,
					Options: []string{"ro", "rbind"},
				}}
			}
			activated = true
		} else if !errdefs.IsNotImplemented(err) {
			return "", fmt.Errorf("activate mounts: %w", err)
		}
	}
	if len(mounts) == 0 {
		if activated && mgr != nil {
			_ = mgr.Deactivate(ctx, activationKey)
		}
		return "", fmt.Errorf("snapshot activation returned no system mounts")
	}
	if err := mount.All(mounts, mountPoint); err != nil {
		if activated && mgr != nil {
			_ = mgr.Deactivate(ctx, activationKey)
		}
		return "", err
	}
	if activated {
		return activationKey, nil
	}
	return "", nil
}

// prepareAndRegisterSnapshot prepares a snapshot, mounts it, and registers it as an active runtime snapshot.
func (ops *snapshotOps) prepareAndRegisterSnapshot(
	ctx context.Context,
	locator SnapshotLocator,
	mountPoint string,
	opts ...snapshots.Opt,
) (*snapshotCleaner, error) {
	var err error

	cleaner := &snapshotCleaner{
		ctx:        ctx,
		namespace:  locator.Namespace,
		key:        locator.Key,
		mountPoint: mountPoint,
		accessPath: mountPoint,
	}

	defer func() {
		if err != nil {
			cleaner.Cleanup()
		}
	}()

	mounts, err := ops.server.snt.Prepare(ctx, locator.Namespace, locator.Key, locator.Parent, opts...)
	if err != nil {
		return nil, err
	}
	cleaner.prepared = true

	if err = ops.server.mkdirAll(mountPoint, common.DirMode); err != nil {
		return nil, err
	}
	cleaner.dirCreated = true

	activationKey := mountActivationKey("active", locator.Namespace, locator.Key)
	if cleaner.activationKey, err = activateAndMount(ctx, ops.server.mountMgr, locator.Namespace, activationKey, mounts, mountPoint); err != nil {
		return nil, fmt.Errorf("mount snapshot %v failed: %v", locator.Key, err)
	}
	cleaner.mountMgr = ops.server.mountMgr
	cleaner.mounted = true
	if len(mounts) == 1 && filepath.IsAbs(mounts[0].Source) {
		cleaner.accessPath = mounts[0].Source
	}

	result, statErr := ops.server.snt.Stat(ctx, locator.Namespace, locator.Key)
	if statErr != nil {
		err = statErr
		return nil, err
	}
	ops.server.addActiveSnapshot(locator.Namespace, locator.Key, &result)
	return cleaner, nil
}

func (ops *snapshotOps) prepareRootfsSnapshot(ctx context.Context, locator SnapshotLocator, opts ...snapshots.Opt) ([]string, error) {
	if ops.server.rootfsSnt == nil {
		return nil, fmt.Errorf("rootfs erofs snapshotter is not configured")
	}
	mounts, err := ops.server.rootfsSnt.Prepare(ctx, locator.Namespace, locator.Key, locator.Parent, opts...)
	if err != nil {
		return nil, err
	}
	result, statErr := ops.server.rootfsSnt.Stat(ctx, locator.Namespace, locator.Key)
	if statErr != nil {
		_ = ops.server.rootfsSnt.Remove(ctx, locator.Namespace, locator.Key)
		return nil, statErr
	}
	ops.server.addActiveSnapshot(locator.Namespace, locator.Key, &result)
	pmemFiles, err := pmemFilesFromErofsMounts(mounts)
	if err != nil {
		_ = ops.server.rootfsSnt.Remove(ctx, locator.Namespace, locator.Key)
		return nil, err
	}
	return pmemFiles, nil
}

// viewSnapshot acquires a shared view mount for a committed snapshot.
func (ops *snapshotOps) viewSnapshot(
	ctx context.Context,
	namespace, parentID, viewAliasKey, viewSnapshotKey, mountPoint string,
	opts ...snapshots.Opt,
) (*snapshotCleaner, error) {
	if parentID == "" {
		return nil, fmt.Errorf("view requires parent snapshot for %s/%s", namespace, viewAliasKey)
	}
	cleaner, _, _, err := ops.server.viewMgr.acquireViewMount(
		ops.server.snt,
		ops.server.mountMgr,
		ctx,
		namespace,
		parentID,
		viewAliasKey,
		viewSnapshotKey,
		mountPoint,
		opts...,
	)
	return cleaner, err
}

func (ops *snapshotOps) viewRootfsSnapshot(
	ctx context.Context,
	namespace, parentID, viewAliasKey, viewSnapshotKey, mountPoint string,
	opts ...snapshots.Opt,
) (*snapshotCleaner, []string, error) {
	if parentID == "" {
		return nil, nil, fmt.Errorf("view requires parent snapshot for %s/%s", namespace, viewAliasKey)
	}
	cleaner, mounts, _, err := ops.server.viewMgr.acquireViewMount(
		ops.server.rootfsSnt,
		ops.server.mountMgr,
		ctx,
		namespace,
		parentID,
		viewAliasKey,
		viewSnapshotKey,
		mountPoint,
		opts...,
	)
	if err != nil {
		return nil, nil, err
	}
	pmemFiles, err := pmemFilesFromErofsMounts(mounts)
	if err != nil {
		cleaner.Cleanup()
		return nil, nil, err
	}
	if err := alignRootfsPmemFiles(pmemFiles); err != nil {
		cleaner.Cleanup()
		return nil, nil, err
	}
	return cleaner, pmemFiles, nil
}

func (ops *snapshotOps) alignCommittedRootfsSnapshot(ctx context.Context, namespace, rootfsSnapshotID string) error {
	viewAliasKey := getRootfsViewAliasKey("align-" + snapshotPathName(rootfsSnapshotID))
	viewSnapshotKey := getSharedViewSnapshotKey(common.SnapshotMountRootfs, rootfsSnapshotID)
	mountPoint := getSharedMountPath(ops.server.workDir, namespace, rootfsSnapshotID)
	cleaner, _, err := ops.viewRootfsSnapshot(ctx, namespace, rootfsSnapshotID, viewAliasKey, viewSnapshotKey, mountPoint)
	if err != nil {
		return err
	}
	cleaner.Cleanup()
	return nil
}

// buildCommitConfigs prepares configuration objects for commit operation.
func (ops *snapshotOps) buildCommitConfigs(
	ctx context.Context,
	namespace, key, memKey, snapshotID, memSnapshotID, parentVMSnapshotID string,
	si *snapshots.Info,
	opts []Opt,
) (*SnapshotConfig, *SnapshotConfig, error) {
	conf := &SnapshotConfig{
		Rootfs: getActiveMountPath(ops.server.workDir, namespace, key, common.SnapshotMountRootfs),
		MemDir: getActiveMountPath(ops.server.workDir, namespace, key, common.SnapshotMountMem),
		VmDir:  getSharedMountPath(ops.server.workDir, namespace, parentVMSnapshotID),
	}
	conf.initDefaults()
	mergeLabels(si, conf)
	for _, o := range opts {
		o(conf)
	}
	conf.createLabels()

	var err error
	conf.pmemFiles = ops.server.getActiveRootfsPmem(namespace, key)
	if len(conf.pmemFiles) == 0 {
		conf.pmemFiles, err = ops.server.resolveRootfsPmemFiles(ctx, namespace, key)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve rootfs erofs pmem files failed: %v", err)
		}
	}

	viewConf := &SnapshotConfig{
		Rootfs:    getSharedMountPath(ops.server.workDir, namespace, snapshotID),
		MemDir:    getSharedMountPath(ops.server.workDir, namespace, memSnapshotID),
		VmDir:     getSharedMountPath(ops.server.workDir, namespace, parentVMSnapshotID),
		pmemFiles: conf.pmemFiles,
	}
	viewConf.initDefaults()

	return conf, viewConf, nil
}

// commitRootfsSnapshot commits the rootfs snapshot with appropriate labels.
func (ops *snapshotOps) commitRootfsSnapshot(ctx context.Context, namespace, key, rootfsSnapshotID string, conf *SnapshotConfig, memSnapshotID, parentVMSnapshotID string, activeInfo *snapshots.Info) (string, error) {
	sourceLabels := make(map[string]string)
	if activeInfo != nil && activeInfo.Parent != "" {
		if parentInfo, statErr := ops.server.rootfsSnt.Stat(ctx, namespace, activeInfo.Parent); statErr == nil {
			for _, label := range []string{common.SnapshotLabelRootfsImage, common.SnapshotLabelRootfsManifest} {
				if v := parentInfo.Labels[label]; v != "" {
					sourceLabels[label] = v
				}
			}
		}
	}

	err := ops.server.rootfsSnt.Commit(ctx, namespace, key, rootfsSnapshotID, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for k, v := range sourceLabels {
			info.Labels[k] = v
		}
		for k, v := range conf.Labels {
			info.Labels[k] = v
		}
		info.Labels[common.SnapshotLabelMemSnapshot] = memSnapshotID
		info.Labels[common.SnapshotLabelVMSnapshot] = parentVMSnapshotID
		return nil
	})
	if err == nil {
		if alignErr := ops.alignCommittedRootfsSnapshot(ctx, namespace, rootfsSnapshotID); alignErr != nil {
			return "", fmt.Errorf("align committed rootfs snapshot %s: %w", rootfsSnapshotID, alignErr)
		}
		return rootfsSnapshotID, nil
	}
	if activeInfo == nil || activeInfo.Parent == "" || !strings.Contains(err.Error(), "not found") {
		return "", err
	}

	parentInfo, statErr := ops.server.rootfsSnt.Stat(ctx, namespace, activeInfo.Parent)
	if statErr != nil {
		return "", err
	}
	if parentInfo.Labels == nil {
		parentInfo.Labels = make(map[string]string)
	}
	var fieldpaths []string
	for k, v := range conf.Labels {
		parentInfo.Labels[k] = v
		fieldpaths = append(fieldpaths, "labels."+k)
	}
	parentInfo.Labels[common.SnapshotLabelMemSnapshot] = memSnapshotID
	parentInfo.Labels[common.SnapshotLabelVMSnapshot] = parentVMSnapshotID
	fieldpaths = append(fieldpaths,
		"labels."+common.SnapshotLabelMemSnapshot,
		"labels."+common.SnapshotLabelVMSnapshot,
	)
	if _, updateErr := ops.server.rootfsSnt.Update(ctx, namespace, parentInfo, fieldpaths...); updateErr != nil {
		return "", fmt.Errorf("update native rootfs snapshot labels %s: %w", activeInfo.Parent, updateErr)
	}
	ops.server.removeActiveSnapshot(namespace, key)
	return activeInfo.Parent, nil
}

// commitMemSnapshot commits the mem snapshot with back-reference to rootfs.
func (ops *snapshotOps) commitMemSnapshot(ctx context.Context, namespace, memKey, memSnapshotID, rootfsSnapshotID string) error {
	err := ops.server.snt.Commit(ctx, namespace, memKey, memSnapshotID, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[common.SnapshotLabelRootfsSnapshot] = rootfsSnapshotID
		return nil
	})
	if err != nil {
		return fmt.Errorf("commit mem snapshot failed: %v", err)
	}
	return nil
}

// prewarmViewMounts pre-creates shared rootfs/vm view mounts for fast restore.
// Prewarmed mounts are kept with refCount=0 and promoted on first real use.
func (ops *snapshotOps) prewarmViewMounts(ctx context.Context, namespace, rootfsSnapshotID, parentVMSnapshotID string, viewConf *SnapshotConfig) error {
	prewarmItems := []struct {
		mountKind  string
		parentID   string
		mountPoint string
	}{
		{common.SnapshotMountVM, parentVMSnapshotID, viewConf.VmDir},
	}

	for _, item := range prewarmItems {
		viewSnapshotKey := getSharedViewSnapshotKey(item.mountKind, item.parentID)
		viewErr := ops.server.viewMgr.ensureViewMount(
			ops.server.snt,
			ops.server.mountMgr,
			ctx,
			namespace,
			item.parentID,
			viewSnapshotKey,
			item.mountPoint,
		)
		if viewErr != nil {
			return fmt.Errorf("prewarm view for %s failed: %w", item.parentID, viewErr)
		}
	}
	return nil
}

// tryRemoveSnapshot attempts to remove a snapshot if it exists.
func (ops *snapshotOps) tryRemoveSnapshot(ctx context.Context, namespace, key string) error {
	if ops.server.rootfsSnt != nil {
		if _, err := ops.server.rootfsSnt.Stat(ctx, namespace, key); err == nil {
			if err := ops.server.rootfsSnt.Remove(ctx, namespace, key); err != nil {
				return fmt.Errorf("remove rootfs snapshot %s: %w", key, err)
			}
			ops.server.removeActiveSnapshot(namespace, key)
			return nil
		}
	}
	if _, err := ops.server.snt.Stat(ctx, namespace, key); err == nil {
		if err := ops.server.snt.Remove(ctx, namespace, key); err != nil {
			return fmt.Errorf("remove snapshot %s: %w", key, err)
		}
	}
	ops.server.removeActiveSnapshot(namespace, key)
	return nil
}

// unmountPath unmounts a filesystem path.
func (ops *snapshotOps) unmountPath(path string) error {
	return ops.server.unmountPath(path)
}
