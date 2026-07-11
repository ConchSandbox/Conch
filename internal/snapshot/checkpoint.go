package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

func (s *Server) commitCheckpointBootLayout(
	ctx context.Context,
	namespace, snapshotID, runtimeKey, capturePath, parentVMSnapshotID string,
) (_ string, retErr error) {
	if strings.TrimSpace(snapshotID) == "" {
		return "", fmt.Errorf("rootfs snapshot id is required (compute externally)")
	}
	captureInfo, err := os.Stat(capturePath)
	if err != nil {
		return "", fmt.Errorf("stat checkpoint capture %s: %w", capturePath, err)
	}
	if !captureInfo.IsDir() {
		return "", fmt.Errorf("checkpoint capture %s is not a directory", capturePath)
	}
	captureConfig := filepath.Join(capturePath, common.SnapshotConfigFileName)
	pmemDeviceCount, err := readSnapshotPmemDeviceCount(captureConfig)
	if err != nil {
		return "", fmt.Errorf("read checkpoint snapshot config: %w", err)
	}

	runtimeRootfs := s.getActiveSnapshot(namespace, runtimeKey)
	if runtimeRootfs == nil {
		return "", fmt.Errorf("snapshot [%s:%s] not found", namespace, runtimeKey)
	}
	if strings.TrimSpace(runtimeRootfs.Parent) == "" {
		return "", fmt.Errorf("active rootfs snapshot [%s:%s] has no committed parent", namespace, runtimeKey)
	}
	runtimeMemKey := getMemKeyFromRootfs(runtimeKey)
	if s.getActiveSnapshot(namespace, runtimeMemKey) == nil {
		return "", fmt.Errorf("mem snapshot [%s:%s] not found", namespace, runtimeMemKey)
	}
	parentVMSnapshotID = strings.TrimSpace(parentVMSnapshotID)
	if parentVMSnapshotID == "" {
		return "", fmt.Errorf("parent VM snapshot id is required")
	}
	memSnapshotID, err := CalculateSnapshotID(namespace, snapshotID+common.MemKeySuffix, "")
	if err != nil {
		return "", fmt.Errorf("calculate mem snapshot id failed: %w", err)
	}

	runtimeLayout := &BootLayout{
		RootfsMount: getActiveMountPath(s.workDir, namespace, runtimeKey, common.SnapshotMountRootfs),
		MemMount:    getActiveMountPath(s.workDir, namespace, runtimeKey, common.SnapshotMountMem),
		VMMount:     getActiveMountPath(s.workDir, namespace, runtimeKey, common.SnapshotMountVM),
	}
	runtimeLayout.initDefaults()
	labels := bootLayoutLabels(runtimeLayout, mergeLabels(runtimeRootfs, runtimeLayout))

	pathName := snapshotPathName(snapshotID)
	rootfsActiveKey := "checkpoint-rootfs-" + pathName
	memActiveKey := "checkpoint-mem-" + pathName
	rootfsLayout := &BootLayout{}
	rootfsLayout.initDefaults()
	if err := s.prepareRootfsSnapshot(
		ctx,
		namespace,
		rootfsActiveKey,
		runtimeRootfs.Parent,
		rootfsLayout,
		withLabels(labels),
	); err != nil {
		return "", fmt.Errorf("prepare checkpoint rootfs snapshot: %w", err)
	}
	rootfsActive := true
	defer func() {
		if !rootfsActive {
			return
		}
		if cleanupErr := s.tryRemoveSnapshotKind(ctx, namespace, rootfsActiveKey, snapshots.KindActive); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	if err := s.commitCheckpointRootfsSnapshot(
		ctx,
		namespace,
		rootfsActiveKey,
		snapshotID,
		labels,
		runtimeRootfs,
	); err != nil {
		return "", err
	}
	rootfsActive = false
	s.removeActiveSnapshot(namespace, rootfsActiveKey)
	rootfsCommitted := true
	defer func() {
		if !rootfsCommitted || retErr == nil {
			return
		}
		if cleanupErr := s.snt.Remove(ctx, namespace, snapshotID); cleanupErr != nil && !errdefs.IsNotFound(cleanupErr) {
			retErr = errors.Join(retErr, fmt.Errorf("remove incomplete checkpoint rootfs %s: %w", snapshotID, cleanupErr))
		}
	}()

	checkpointPmemFiles, err := s.resolveRootfsPmemFiles(ctx, namespace, snapshotID)
	if err != nil {
		return "", fmt.Errorf("resolve checkpoint rootfs pmem files: %w", err)
	}
	checkpointPmemFiles, err = selectSnapshotRestorePmemFiles(checkpointPmemFiles, pmemDeviceCount)
	if err != nil {
		return "", fmt.Errorf("select checkpoint rootfs pmem files: %w", err)
	}

	memMountPoint := getActiveMountPath(s.workDir, namespace, memActiveKey, common.SnapshotMountMem)
	memAccessPath, err := s.prepareAndMountActiveSnapshot(ctx, namespace, memActiveKey, "", memMountPoint)
	if err != nil {
		return "", fmt.Errorf("prepare checkpoint mem snapshot: %w", err)
	}
	memActive := true
	memMounted := true
	defer func() {
		if !memActive {
			return
		}
		if memMounted {
			if cleanupErr := s.unmountAndDeactivateMount(ctx, namespace, "active", memActiveKey, memMountPoint); cleanupErr != nil {
				retErr = errors.Join(retErr, cleanupErr)
			}
		}
		if cleanupErr := s.tryRemoveSnapshotKind(ctx, namespace, memActiveKey, snapshots.KindActive); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	checkpointLayout := &BootLayout{
		MemMount:     memAccessPath,
		MemorySizeMB: runtimeLayout.MemorySizeMB,
		SnapshotDir:  runtimeLayout.SnapshotDir,
	}
	checkpointLayout.initDefaults()
	if err := ensureMemFile(checkpointLayout, memAccessPath, true); err != nil {
		return "", fmt.Errorf("prepare checkpoint mem.img: %w", err)
	}
	if err := replaceCheckpointCapture(capturePath, checkpointLayout.SnapDir()); err != nil {
		return "", fmt.Errorf("stage checkpoint capture: %w", err)
	}
	checkpointConfig := filepath.Join(checkpointLayout.SnapDir(), common.SnapshotConfigFileName)
	if err := (&configUpdater{}).updateSnapshotPmemPaths(checkpointConfig, checkpointPmemFiles); err != nil {
		return "", fmt.Errorf("update checkpoint pmem paths: %w", err)
	}

	if err := s.unmountAndDeactivateMount(ctx, namespace, "active", memActiveKey, memMountPoint); err != nil {
		return "", fmt.Errorf("unmount checkpoint mem snapshot: %w", err)
	}
	memMounted = false
	if err := s.commitMemSnapshot(ctx, namespace, memActiveKey, memSnapshotID); err != nil {
		return "", err
	}
	memActive = false
	s.removeActiveSnapshot(namespace, memActiveKey)
	rootfsCommitted = false
	return snapshotID, nil
}

func (s *Server) commitCheckpointRootfsSnapshot(
	ctx context.Context,
	namespace, activeKey, snapshotID string,
	labels map[string]string,
	runtimeRootfs *snapshots.Info,
) error {
	sourceLabels := make(map[string]string)
	if runtimeRootfs != nil && runtimeRootfs.Parent != "" {
		if parentInfo, err := s.snt.Stat(ctx, namespace, runtimeRootfs.Parent); err == nil {
			for _, label := range []string{common.SnapshotLabelRootfsImage, common.SnapshotLabelRootfsManifest} {
				if value := parentInfo.Labels[label]; value != "" {
					sourceLabels[label] = value
				}
			}
		}
	}
	if err := s.snt.Commit(ctx, namespace, activeKey, snapshotID, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for key, value := range sourceLabels {
			info.Labels[key] = value
		}
		for key, value := range labels {
			info.Labels[key] = value
		}
		return nil
	}); err != nil {
		return fmt.Errorf("commit checkpoint rootfs snapshot: %w", err)
	}
	if err := s.alignCommittedRootfsSnapshot(ctx, namespace, snapshotID); err != nil {
		alignErr := fmt.Errorf("align committed rootfs snapshot %s: %w", snapshotID, err)
		if removeErr := s.snt.Remove(ctx, namespace, snapshotID); removeErr != nil && !errdefs.IsNotFound(removeErr) {
			return errors.Join(alignErr, fmt.Errorf("remove unaligned rootfs snapshot %s: %w", snapshotID, removeErr))
		}
		return alignErr
	}
	return nil
}

func replaceCheckpointCapture(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return copyCheckpointTree(src, dst)
}

func copyCheckpointTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		case entry.Type().IsRegular():
			return copyCheckpointFile(path, target, info.Mode().Perm())
		default:
			return nil
		}
	})
}

func copyCheckpointFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), common.DirMode); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
