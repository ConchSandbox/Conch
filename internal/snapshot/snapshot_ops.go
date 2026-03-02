package snapshot

import (
	"context"
	"fmt"
	"log/slog"

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
		server:     ops.server,
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

// viewSnapshot acquires a view mount for a committed snapshot.
func (ops *snapshotOps) viewSnapshot(
	ctx context.Context,
	namespace, parentID, viewKey, mountPoint string,
	opts ...snapshots.Opt,
) (*snapshotCleaner, error) {
	if parentID == "" {
		return nil, fmt.Errorf("view requires parent snapshot for %s/%s", namespace, viewKey)
	}
	cleaner, _, err := ops.server.viewMgr.acquireViewMount(ops.server.snt, ctx, namespace, parentID, viewKey, mountPoint, opts...)
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
		Rootfs: getMountPath(ops.server.workDir, namespace, key),
		MemDir: getMountPath(ops.server.workDir, namespace, memKey),
		VmDir:  getMountPath(ops.server.workDir, namespace, parentVMSnapshotID),
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
		Rootfs:    getMountPath(ops.server.workDir, namespace, snapshotID),
		MemDir:    getMountPath(ops.server.workDir, namespace, memSnapshotID),
		VmDir:     getMountPath(ops.server.workDir, namespace, parentVMSnapshotID),
		pmemFiles: conf.pmemFiles,
	}
	viewConf.initDefaults()

	return conf, viewConf, nil
}

// commitRootfsSnapshot commits the rootfs snapshot with appropriate labels.
func (ops *snapshotOps) commitRootfsSnapshot(ctx context.Context, namespace, key, rootfsSnapshotID string, conf *SnapshotConfig, memSnapshotID, parentVMSnapshotID string) error {
	return ops.server.snt.Commit(ctx, namespace, key, rootfsSnapshotID, noGcOpt, func(info *snapshots.Info) error {
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
	err := ops.server.snt.Commit(ctx, namespace, memKey, memSnapshotID, noGcOpt, func(info *snapshots.Info) error {
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

// updateSnapshotCache updates the server's snapshot cache with newly committed snapshots.
// Returns list of successfully added snapshot IDs for rollback purposes.
func (ops *snapshotOps) updateSnapshotCache(ctx context.Context, namespace, rootfsSnapshotID, memSnapshotID string) ([]string, error) {
	var addedSnapshots []string
	for _, sid := range []string{rootfsSnapshotID, memSnapshotID} {
		result, statErr := ops.server.snt.Stat(ctx, namespace, sid)
		if statErr != nil {
			return addedSnapshots, fmt.Errorf("stat committed snapshot %s failed: %v", sid, statErr)
		}
		ops.server.addSnapshot(namespace, sid, &result)
		addedSnapshots = append(addedSnapshots, sid)
	}
	return addedSnapshots, nil
}

// prewarmViewMounts pre-creates view mounts for fast subsequent restore operations.
// Rolls back any partially created views on failure.
func (ops *snapshotOps) prewarmViewMounts(ctx context.Context, namespace, rootfsSnapshotID, memSnapshotID, parentVMSnapshotID string, viewConf *SnapshotConfig) error {
	prewarmItems := []struct {
		parentID   string
		mountPoint string
	}{
		{rootfsSnapshotID, viewConf.Rootfs},
		{parentVMSnapshotID, viewConf.VmDir},
		{memSnapshotID, viewConf.MemDir},
	}

	var prewarmKeys []string
	for _, item := range prewarmItems {
		viewKey := common.TempViewPrefix + item.parentID
		_, _, viewErr := ops.server.viewMgr.acquireViewMount(ops.server.snt, ctx, namespace, item.parentID, viewKey, item.mountPoint, noGcOpt)
		if viewErr != nil {
			if len(prewarmKeys) > 0 {
				if _, releaseErr := ops.server.viewMgr.releaseViewAliases(ops.server.snt, namespace, prewarmKeys...); releaseErr != nil {
					slog.Warn("prewarm rollback had errors", "err", releaseErr)
				}
			}
			slog.Warn("prewarm failed, rolled back views", "rolledBack", len(prewarmKeys), "err", viewErr)
			return fmt.Errorf("prewarm view for %s failed: %w", item.parentID, viewErr)
		}
		prewarmKeys = append(prewarmKeys, viewKey)
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
