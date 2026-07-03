package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

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

func unmountAndDeactivate(ctx context.Context, mgr mount.Manager, namespace, activationKey, mountPoint string) error {
	var errs []error
	if err := mount.UnmountAll(mountPoint, unix.MNT_FORCE); err != nil {
		errs = append(errs, fmt.Errorf("unmount %s: %w", mountPoint, err))
	}
	if activationKey != "" && mgr != nil {
		if namespace != "" {
			ctx = namespaces.WithNamespace(ctx, namespace)
		}
		if err := mgr.Deactivate(ctx, activationKey); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deactivate mount %s: %w", activationKey, err))
		}
	}
	return errors.Join(errs...)
}

// prepareAndMountActiveSnapshot prepares and mounts an active snapshot, then records it in the runtime cache.
// It returns the path callers should use to access the mounted snapshot.
func (s *Server) prepareAndMountActiveSnapshot(
	ctx context.Context,
	namespace, key, parent string,
	mountPoint string,
	opts ...snapshots.Opt,
) (_ string, err error) {
	accessPath := mountPoint

	mounts, err := s.snt.Prepare(ctx, namespace, key, parent, opts...)
	if err != nil {
		return "", err
	}
	defer func() {
		if err == nil || s.snt == nil {
			return
		}
		if removeErr := s.snt.Remove(ctx, namespace, key); removeErr != nil && !errdefs.IsNotFound(removeErr) {
			err = errors.Join(err, fmt.Errorf("remove snapshot %s: %w", key, removeErr))
		}
	}()

	if err = os.MkdirAll(mountPoint, common.DirMode); err != nil {
		return "", err
	}
	defer func() {
		if err == nil {
			return
		}
		if removeErr := os.RemoveAll(mountPoint); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove dir %s: %w", mountPoint, removeErr))
		} else if pruneErr := cleanupEmptySnapshotParents(mountPoint); pruneErr != nil {
			err = errors.Join(err, fmt.Errorf("prune empty parent dirs for %s: %w", mountPoint, pruneErr))
		}
	}()

	activationKey := mountActivationKey("active", namespace, key)
	if activationKey, err = activateAndMount(ctx, s.mountMgr, namespace, activationKey, mounts, mountPoint); err != nil {
		return "", fmt.Errorf("mount snapshot %v failed: %v", key, err)
	}
	defer func() {
		if err == nil {
			return
		}
		if cleanupErr := unmountAndDeactivate(ctx, s.mountMgr, namespace, activationKey, mountPoint); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if len(mounts) == 1 && filepath.IsAbs(mounts[0].Source) {
		accessPath = mounts[0].Source
	}

	result, statErr := s.snt.Stat(ctx, namespace, key)
	if statErr != nil {
		err = statErr
		return "", err
	}
	s.addActiveSnapshot(namespace, key, &result)
	return accessPath, nil
}

func (s *Server) prepareRootfsSnapshot(ctx context.Context, namespace, key, parent string, layout *BootLayout, opts ...snapshots.Opt) error {
	if s.snt == nil {
		return fmt.Errorf("rootfs erofs snapshotter is not configured")
	}
	if layout == nil {
		return fmt.Errorf("rootfs snapshot layout is nil")
	}
	mounts, err := s.snt.Prepare(ctx, namespace, key, parent, opts...)
	if err != nil {
		return err
	}
	result, statErr := s.snt.Stat(ctx, namespace, key)
	if statErr != nil {
		_ = s.snt.Remove(ctx, namespace, key)
		return statErr
	}
	pmemFiles, err := pmemFilesFromErofsMounts(mounts)
	if err != nil {
		_ = s.snt.Remove(ctx, namespace, key)
		return err
	}
	layout.pmemFiles = pmemFiles
	s.addActiveSnapshot(namespace, key, &result)
	s.addActiveRootfsPmem(namespace, key, layout.pmemFiles)
	return nil
}

func (s *Server) viewRootfsSnapshot(
	ctx context.Context,
	namespace, parentID, viewAliasKey, viewSnapshotKey, mountPoint string,
	opts ...snapshots.Opt,
) ([]string, error) {
	if parentID == "" {
		return nil, fmt.Errorf("view requires parent snapshot for %s/%s", namespace, viewAliasKey)
	}
	mounts, err := s.viewMgr.acquireViewMount(
		s.snt,
		s.mountMgr,
		ctx,
		namespace,
		parentID,
		viewAliasKey,
		viewSnapshotKey,
		mountPoint,
		opts...,
	)
	if err != nil {
		return nil, err
	}
	releaseOnError := func(cause error) error {
		_, releaseErr := s.viewMgr.releaseViewAliases(s.snt, s.mountMgr, namespace, viewAliasKey)
		return errors.Join(cause, releaseErr)
	}
	pmemFiles, err := pmemFilesFromErofsMounts(mounts)
	if err != nil {
		return nil, releaseOnError(err)
	}
	if err := alignRootfsPmemFiles(pmemFiles); err != nil {
		return nil, releaseOnError(err)
	}
	return pmemFiles, nil
}

func (s *Server) alignCommittedRootfsSnapshot(ctx context.Context, namespace, rootfsSnapshotID string) error {
	viewAliasKey := getRootfsViewAliasKey("align-" + snapshotPathName(rootfsSnapshotID))
	viewSnapshotKey := getSharedViewSnapshotKey(common.SnapshotMountRootfs, rootfsSnapshotID)
	mountPoint := getSharedMountPath(s.workDir, namespace, rootfsSnapshotID)
	if _, err := s.viewRootfsSnapshot(ctx, namespace, rootfsSnapshotID, viewAliasKey, viewSnapshotKey, mountPoint); err != nil {
		return err
	}
	if _, err := s.viewMgr.releaseViewAliases(s.snt, s.mountMgr, namespace, viewAliasKey); err != nil {
		return fmt.Errorf("release rootfs alignment view %s: %w", rootfsSnapshotID, err)
	}
	return nil
}

func (s *Server) loadCommittedBootLayoutMetadata(ctx context.Context, namespace string, parents ParentSnapshotIDs, layout *BootLayout) (bool, error) {
	if parents.Rootfs == "" {
		return false, nil
	}
	if s.snt == nil {
		return false, fmt.Errorf("rootfs erofs snapshotter is not configured")
	}
	rootfsInfo, err := s.snt.Stat(ctx, namespace, parents.Rootfs)
	if err != nil {
		return false, fmt.Errorf("stat rootfs snapshot metadata %s: %w", parents.Rootfs, err)
	}

	memorySizeFromSnapshot := false
	if v := rootfsInfo.Labels[common.SnapshotLabelMemSize]; v != "" {
		memorySizeMB, parseErr := strconv.ParseInt(v, 10, 64)
		if parseErr == nil && memorySizeMB > 0 {
			layout.MemorySizeMB = memorySizeMB
			memorySizeFromSnapshot = true
		}
	}
	if v := rootfsInfo.Labels[common.SnapshotLabelSnapshotDir]; v != "" {
		layout.SnapshotDir = v
	}
	return memorySizeFromSnapshot, nil
}

// buildCommitConfigs prepares configuration objects for commit operation.
func (s *Server) buildCommitConfigs(
	ctx context.Context,
	namespace, key, memKey, snapshotID, memSnapshotID, parentVMSnapshotID string,
	si *snapshots.Info,
) (*BootLayout, map[string]string, *BootLayout, error) {
	layout := &BootLayout{
		RootfsMount: getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountRootfs),
		MemMount:    getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountMem),
		VMMount:     getSharedMountPath(s.workDir, namespace, parentVMSnapshotID),
	}
	layout.initDefaults()
	labels := mergeLabels(si, layout)
	labels = bootLayoutLabels(layout, labels)

	var err error
	layout.pmemFiles = s.getActiveRootfsPmem(namespace, key)
	if len(layout.pmemFiles) == 0 {
		layout.pmemFiles, err = s.resolveRootfsPmemFiles(ctx, namespace, key)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve rootfs erofs pmem files failed: %v", err)
		}
	}

	viewLayout := &BootLayout{
		RootfsMount: getSharedMountPath(s.workDir, namespace, snapshotID),
		MemMount:    getSharedMountPath(s.workDir, namespace, memSnapshotID),
		VMMount:     getSharedMountPath(s.workDir, namespace, parentVMSnapshotID),
		pmemFiles:   layout.pmemFiles,
	}
	viewLayout.initDefaults()

	return layout, labels, viewLayout, nil
}

// commitRootfsSnapshot commits the rootfs snapshot with appropriate labels.
func (s *Server) commitRootfsSnapshot(ctx context.Context, namespace, key, rootfsSnapshotID string, labels map[string]string, memSnapshotID, parentVMSnapshotID string, activeInfo *snapshots.Info) (string, error) {
	sourceLabels := make(map[string]string)
	if activeInfo != nil && activeInfo.Parent != "" {
		if parentInfo, statErr := s.snt.Stat(ctx, namespace, activeInfo.Parent); statErr == nil {
			for _, label := range []string{common.SnapshotLabelRootfsImage, common.SnapshotLabelRootfsManifest} {
				if v := parentInfo.Labels[label]; v != "" {
					sourceLabels[label] = v
				}
			}
		}
	}

	err := s.snt.Commit(ctx, namespace, key, rootfsSnapshotID, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for k, v := range sourceLabels {
			info.Labels[k] = v
		}
		for k, v := range labels {
			info.Labels[k] = v
		}
		info.Labels[common.SnapshotLabelGroupMemRef] = memSnapshotID
		info.Labels[common.SnapshotLabelGroupVMRef] = parentVMSnapshotID
		return nil
	})
	if err == nil {
		if alignErr := s.alignCommittedRootfsSnapshot(ctx, namespace, rootfsSnapshotID); alignErr != nil {
			return "", fmt.Errorf("align committed rootfs snapshot %s: %w", rootfsSnapshotID, alignErr)
		}
		return rootfsSnapshotID, nil
	}
	if activeInfo == nil || activeInfo.Parent == "" || !strings.Contains(err.Error(), "not found") {
		return "", err
	}

	parentInfo, statErr := s.snt.Stat(ctx, namespace, activeInfo.Parent)
	if statErr != nil {
		return "", err
	}
	if parentInfo.Labels == nil {
		parentInfo.Labels = make(map[string]string)
	}
	var fieldpaths []string
	for k, v := range labels {
		parentInfo.Labels[k] = v
		fieldpaths = append(fieldpaths, "labels."+k)
	}
	parentInfo.Labels[common.SnapshotLabelGroupMemRef] = memSnapshotID
	parentInfo.Labels[common.SnapshotLabelGroupVMRef] = parentVMSnapshotID
	fieldpaths = append(fieldpaths,
		"labels."+common.SnapshotLabelGroupMemRef,
		"labels."+common.SnapshotLabelGroupVMRef,
	)
	if _, updateErr := s.snt.Update(ctx, namespace, parentInfo, fieldpaths...); updateErr != nil {
		return "", fmt.Errorf("update native rootfs snapshot labels %s: %w", activeInfo.Parent, updateErr)
	}
	s.removeActiveSnapshot(namespace, key)
	return activeInfo.Parent, nil
}

// commitMemSnapshot commits the mem snapshot with back-reference to rootfs.
func (s *Server) commitMemSnapshot(ctx context.Context, namespace, memKey, memSnapshotID, rootfsSnapshotID string) error {
	err := s.snt.Commit(ctx, namespace, memKey, memSnapshotID, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[common.SnapshotLabelGroupID] = rootfsSnapshotID
		info.Labels[common.SnapshotLabelComponentKind] = common.SnapshotComponentKindMem
		return nil
	})
	if err != nil {
		return fmt.Errorf("commit mem snapshot failed: %v", err)
	}
	return nil
}

// prewarmViewMounts pre-creates shared rootfs/vm view mounts for fast restore.
// Prewarmed mounts are kept with refCount=0 and promoted on first real use.
func (s *Server) prewarmViewMounts(ctx context.Context, namespace, rootfsSnapshotID, parentVMSnapshotID string, viewLayout *BootLayout) error {
	prewarmItems := []struct {
		mountKind  string
		parentID   string
		mountPoint string
	}{
		{common.SnapshotMountVM, parentVMSnapshotID, viewLayout.VMMount},
	}

	for _, item := range prewarmItems {
		viewSnapshotKey := getSharedViewSnapshotKey(item.mountKind, item.parentID)
		viewErr := s.viewMgr.ensureViewMount(
			s.snt,
			s.mountMgr,
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
func (s *Server) tryRemoveSnapshot(ctx context.Context, namespace, key string) error {
	if _, err := s.snt.Stat(ctx, namespace, key); err == nil {
		if err := s.snt.Remove(ctx, namespace, key); err != nil {
			return fmt.Errorf("remove snapshot %s: %w", key, err)
		}
	}
	s.removeActiveSnapshot(namespace, key)
	return nil
}
