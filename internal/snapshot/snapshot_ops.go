package snapshot

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/snapshots"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

// snapshotOps provides helper operations for snapshot management.
type snapshotOps struct {
	server *server
}

// prepareAndRegisterSnapshot prepares a snapshot, mounts it, and registers in cache.
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

	if len(mounts) != 1 {
		return nil, fmt.Errorf("overlayfs require only one mount info, but get: %v", mounts)
	}

	if err = ops.server.mkdirAll(mountPoint, common.DirMode); err != nil {
		return nil, err
	}
	cleaner.dirCreated = true

	if err = mounts[0].Mount(mountPoint); err != nil {
		return nil, fmt.Errorf("mount snapshot %v failed: %v", locator.Key, err)
	}
	cleaner.mounted = true

	result, statErr := ops.server.snt.Stat(ctx, locator.Namespace, locator.Key)
	if statErr != nil {
		err = statErr
		return nil, err
	}
	ops.server.addSnapshot(locator.Namespace, locator.Key, &result)
	return cleaner, nil
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
	cleaner, _, err := ops.server.viewMgr.acquireViewMount(
		ops.server.snt,
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
	conf.pmemFiles, err = listRootfsLayerErofs(conf.Rootfs)
	if err != nil {
		return nil, nil, fmt.Errorf("list rootfs layer erofs failed: %v", err)
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
func (ops *snapshotOps) commitRootfsSnapshot(ctx context.Context, namespace, key, rootfsSnapshotID string, conf *SnapshotConfig, memSnapshotID, parentVMSnapshotID string) error {
	return ops.server.snt.Commit(ctx, namespace, key, rootfsSnapshotID, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for k, v := range conf.Labels {
			info.Labels[k] = v
		}
		info.Labels[common.SnapshotLabelMemSnapshot] = memSnapshotID
		info.Labels[common.SnapshotLabelVMSnapshot] = parentVMSnapshotID
		return nil
	})
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
		{common.SnapshotMountRootfs, rootfsSnapshotID, viewConf.Rootfs},
		{common.SnapshotMountVM, parentVMSnapshotID, viewConf.VmDir},
	}

	for _, item := range prewarmItems {
		viewSnapshotKey := getSharedViewSnapshotKey(item.mountKind, item.parentID)
		viewErr := ops.server.viewMgr.ensureViewMount(
			ops.server.snt,
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
	if _, err := ops.server.snt.Stat(ctx, namespace, key); err == nil {
		if err := ops.server.snt.Remove(ctx, namespace, key); err != nil {
			return fmt.Errorf("remove snapshot %s: %w", key, err)
		}
	}
	ops.server.removeSnapshot(namespace, key)
	return nil
}

// unmountPath unmounts a filesystem path.
func (ops *snapshotOps) unmountPath(path string) error {
	return ops.server.unmountPath(path)
}
