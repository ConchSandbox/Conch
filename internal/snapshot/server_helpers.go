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

func (s *Server) viewSnapshotMount(
	ctx context.Context,
	namespace, parentID, viewSnapshotKey, mountPoint string,
	opts ...snapshots.Opt,
) (_ []mount.Mount, err error) {
	if parentID == "" {
		return nil, fmt.Errorf("view %s requires parent snapshot", viewSnapshotKey)
	}
	mounts, err := s.snt.View(ctx, viewSnapshotKey, parentID, opts...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err == nil || s.snt == nil {
			return
		}
		if removeErr := s.snt.Remove(ctx, viewSnapshotKey); removeErr != nil && !errdefs.IsNotFound(removeErr) {
			err = errors.Join(err, fmt.Errorf("remove view snapshot %s: %w", viewSnapshotKey, removeErr))
		}
	}()

	if err = os.MkdirAll(mountPoint, common.DirMode); err != nil {
		return nil, err
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

	activationKey := mountActivationKey("view", namespace, viewSnapshotKey)
	activatedKey, err := activateAndMount(ctx, s.mountMgr, namespace, activationKey, mounts, mountPoint)
	if err != nil {
		return nil, fmt.Errorf("mount view snapshot %s failed: %w", viewSnapshotKey, err)
	}
	defer func() {
		if err == nil {
			return
		}
		if cleanupErr := unmountAndDeactivate(ctx, s.mountMgr, namespace, activatedKey, mountPoint); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	return mounts, nil
}

func (s *Server) unmountAndDeactivateMount(ctx context.Context, namespace, activationPrefix, key, mountPoint string) error {
	var errs []error
	if mountPoint != "" {
		if err := s.unmountPath(mountPoint); err != nil {
			errs = append(errs, err)
		}
	}
	if s.mountMgr != nil {
		activationKey := mountActivationKey(activationPrefix, namespace, key)
		if err := s.mountMgr.Deactivate(namespaces.WithNamespace(ctx, namespace), activationKey); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deactivate mount %s: %w", activationKey, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Server) releaseActiveSnapshot(ctx context.Context, namespace, key, mountPoint string) error {
	if err := s.unmountAndDeactivateMount(ctx, namespace, "active", key, mountPoint); err != nil {
		return fmt.Errorf("unmount active snapshot %s failed, skip snapshot remove: %w", key, err)
	}
	return s.tryRemoveSnapshotKind(ctx, namespace, key, snapshots.KindActive)
}

func (s *Server) releaseViewSnapshot(ctx context.Context, namespace, viewSnapshotKey, mountPoint string) error {
	if err := s.unmountAndDeactivateMount(ctx, namespace, "view", viewSnapshotKey, mountPoint); err != nil {
		return fmt.Errorf("unmount view snapshot %s failed, skip snapshot remove: %w", viewSnapshotKey, err)
	}
	return s.tryRemoveSnapshotKind(ctx, namespace, viewSnapshotKey, snapshots.KindView)
}

func (s *Server) activeSnapshotExists(ctx context.Context, namespace, key string) bool {
	if s.getActiveSnapshot(namespace, key) != nil {
		return true
	}
	if s.snt == nil {
		return false
	}
	info, err := s.snt.Stat(ctx, key)
	return err == nil && info.Kind == snapshots.KindActive
}

func (s *Server) snapshotKindExists(ctx context.Context, namespace, key string, kind snapshots.Kind) bool {
	if s.snt == nil {
		return false
	}
	info, err := s.snt.Stat(ctx, key)
	return err == nil && info.Kind == kind
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

	mounts, err := s.snt.Prepare(ctx, key, parent, opts...)
	if err != nil {
		return "", err
	}
	defer func() {
		if err == nil || s.snt == nil {
			return
		}
		if removeErr := s.snt.Remove(ctx, key); removeErr != nil && !errdefs.IsNotFound(removeErr) {
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

	result, statErr := s.snt.Stat(ctx, key)
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
	mounts, err := s.snt.Prepare(ctx, key, parent, opts...)
	if err != nil {
		return err
	}
	result, statErr := s.snt.Stat(ctx, key)
	if statErr != nil {
		_ = s.snt.Remove(ctx, key)
		return statErr
	}
	pmemFiles, err := pmemFilesFromErofsMounts(mounts)
	if err != nil {
		_ = s.snt.Remove(ctx, key)
		return err
	}
	layout.pmemFiles = pmemFiles
	s.addActiveSnapshot(namespace, key, &result)
	s.addActiveRootfsPmem(namespace, key, layout.pmemFiles)
	return nil
}

func (s *Server) viewRootfsSnapshot(
	ctx context.Context,
	namespace, parentID, viewSnapshotKey, mountPoint string,
	opts ...snapshots.Opt,
) ([]string, error) {
	if parentID == "" {
		return nil, fmt.Errorf("view requires parent snapshot for %s/%s", namespace, viewSnapshotKey)
	}
	mounts, err := s.viewSnapshotMount(ctx, namespace, parentID, viewSnapshotKey, mountPoint, opts...)
	if err != nil {
		return nil, err
	}
	releaseOnError := func(cause error) error {
		releaseErr := s.releaseViewSnapshot(ctx, namespace, viewSnapshotKey, mountPoint)
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
	key := "align-" + snapshotPathName(rootfsSnapshotID)
	viewSnapshotKey := getRootfsViewSnapshotKey(key)
	mountPoint := getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountRootfs)
	if _, err := s.viewRootfsSnapshot(ctx, namespace, rootfsSnapshotID, viewSnapshotKey, mountPoint); err != nil {
		return err
	}
	if err := s.releaseViewSnapshot(ctx, namespace, viewSnapshotKey, mountPoint); err != nil {
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
	rootfsInfo, err := s.snt.Stat(ctx, parents.Rootfs)
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
	return memorySizeFromSnapshot, nil
}

// tryRemoveSnapshot attempts to remove a snapshot if it exists.
func (s *Server) tryRemoveSnapshot(ctx context.Context, namespace, key string) error {
	if _, err := s.snt.Stat(ctx, key); err == nil {
		if err := s.snt.Remove(ctx, key); err != nil {
			return fmt.Errorf("remove snapshot %s: %w", key, err)
		}
	}
	s.removeActiveSnapshot(namespace, key)
	return nil
}

func (s *Server) tryRemoveSnapshotKind(ctx context.Context, namespace, key string, allowed ...snapshots.Kind) error {
	activeCached := s.getActiveSnapshot(namespace, key) != nil
	info, err := s.snt.Stat(ctx, key)
	if err != nil {
		s.removeActiveSnapshot(namespace, key)
		return nil
	}
	allowedKind := false
	for _, kind := range allowed {
		if info.Kind == kind || (kind == snapshots.KindActive && activeCached) {
			allowedKind = true
			break
		}
	}
	if !allowedKind {
		return nil
	}
	if err := s.snt.Remove(ctx, key); err != nil {
		return fmt.Errorf("remove snapshot %s: %w", key, err)
	}
	s.removeActiveSnapshot(namespace, key)
	return nil
}
