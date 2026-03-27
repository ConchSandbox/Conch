package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/snapshot/snapshotter"
)

// server manages snapshot lifecycle with caching and view sharing.
type server struct {
	snt       snapshotter.Snapshotter
	snapshots map[string]map[string]*snapshots.Info // only active snapshot
	lock      sync.RWMutex
	workDir   string
	viewMgr   *viewManager
}

var gServer server

// NewServer initializes the snapshot server with containerd client.
func NewServer(workDir string, daemonClient *daemon.Client) error {
	sn, err := snapshotter.NewContainerdSnap(daemonClient)
	if err != nil {
		return err
	}
	gServer.snt = sn
	gServer.workDir = workDir
	gServer.snapshots = make(map[string]map[string]*snapshots.Info)
	gServer.viewMgr = &viewManager{
		viewMounts:  make(map[string]map[string]*viewMountRef),
		viewAliases: make(map[string]map[string]string),
	}

	nss, err := sn.ListNamespaces(context.Background())
	if err != nil {
		return err
	}
	for _, ns := range nss {
		infos := make(map[string]*snapshots.Info)
		err := sn.List(context.Background(), ns, infos)
		if err != nil {
			return fmt.Errorf("list snapshot of %s err: %v", ns, err)
		}
		gServer.snapshots[ns] = infos
	}

	return nil
}

// getSnapshot retrieves snapshot info from cache.
func (s *server) getSnapshot(ns, key string) *snapshots.Info {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if m, ok := s.snapshots[ns]; ok {
		return m[key]
	}
	return nil
}

// addSnapshot adds snapshot info to cache.
func (s *server) addSnapshot(ns, key string, info *snapshots.Info) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if m, ok := s.snapshots[ns]; ok {
		m[key] = info
	} else {
		m := make(map[string]*snapshots.Info)
		m[key] = info
		s.snapshots[ns] = m
	}
}

// removeSnapshot removes snapshot info from cache.
func (s *server) removeSnapshot(ns, key string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if m, ok := s.snapshots[ns]; ok {
		delete(m, key)
	}
}

// mkdirAll creates a directory with common.DirMode permissions.
func (s *server) mkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// unmountPath unmounts a filesystem path and removes the directory.
// Skips if path doesn't exist (may have been cleaned up already).
func (s *server) unmountPath(path string) error {
	// Check if path exists first
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	if err := mount.Unmount(path, unix.MNT_FORCE); err != nil {
		// If unmount fails because path doesn't exist, that's ok
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("unmount %s: %w", path, err)
	}
	// Remove the mount point directory after unmount
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove dir %s: %w", path, err)
	}
	if err := cleanupEmptySnapshotParents(path); err != nil {
		return fmt.Errorf("prune empty parent dirs for %s: %w", path, err)
	}
	return nil
}

// getRelatedSnapshotID retrieves a related snapshot ID by label.
func (s *server) getRelatedSnapshotID(namespace, rootfsSnapshotID, label string) (string, error) {
	info := s.getSnapshot(namespace, rootfsSnapshotID)
	if info == nil {
		return "", fmt.Errorf("rootfs snapshot %s not found", rootfsSnapshotID)
	}
	return info.Labels[label], nil
}

// Prepare creates active snapshots for rootfs/mem and a shared view for vm.
func (s *server) Prepare(
	ctx context.Context,
	namespace, key string,
	parents ParentSnapshotIDs,
	opts ...Opt,
) (_ *SnapshotConfig, err error) {
	if si := s.getSnapshot(namespace, key); si != nil {
		return nil, fmt.Errorf("snapshot [%s:%s] existed", namespace, key)
	}

	memKey := getMemKeyFromRootfs(key)
	vmViewAliasKey := getVMViewAliasKey(key)
	vmViewSnapshotKey := getSharedViewSnapshotKey(common.SnapshotMountVM, parents.VM)

	ops := &snapshotOps{server: s}

	conf := &SnapshotConfig{
		Rootfs: getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountRootfs),
		MemDir: getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountMem),
		VmDir:  getSharedMountPath(s.workDir, namespace, parents.VM),
	}
	conf.initDefaults()
	for _, o := range opts {
		o(conf)
	}
	conf.createLabels()

	type cleanupItem struct {
		key     string
		cleaner *snapshotCleaner
	}
	var activeCleanups []cleanupItem
	var vmCleaner *snapshotCleaner
	defer func() {
		if err != nil {
			for _, item := range activeCleanups {
				s.removeSnapshot(namespace, item.key)
				if item.cleaner != nil {
					item.cleaner.Cleanup()
				}
			}
			if vmCleaner != nil {
				vmCleaner.Cleanup()
			}
		}
	}()

	// Step 1: prepare rootfs
	rootfsCleaner, err := ops.prepareAndRegisterSnapshot(
		ctx,
		NewSnapshotLocator(namespace, key, parents.Rootfs),
		conf.Rootfs,
		withLabels(conf),
	)
	if err != nil {
		return nil, err
	}
	activeCleanups = append(activeCleanups, cleanupItem{key: key, cleaner: rootfsCleaner})

	conf.pmemFiles, err = listRootfsLayerErofs(conf.Rootfs)
	if err != nil {
		return nil, fmt.Errorf("list rootfs layer erofs failed: %v", err)
	}

	// Step 2: view vm (read-only, shared)
	vmCleaner, err = ops.viewSnapshot(ctx, namespace, parents.VM, vmViewAliasKey, vmViewSnapshotKey, conf.VmDir)
	if err != nil {
		return nil, fmt.Errorf("view vm failed: %v", err)
	}

	// Step 3: prepare mem + create sparse memfile
	memCleaner, err := ops.prepareAndRegisterSnapshot(ctx, NewSnapshotLocator(namespace, memKey, parents.Mem), conf.MemDir)
	if err != nil {
		return nil, err
	}
	activeCleanups = append(activeCleanups, cleanupItem{key: memKey, cleaner: memCleaner})

	if err = ensureMemFile(conf, conf.MemDir, true); err != nil {
		return nil, fmt.Errorf("prepare mem.img failed: %v", err)
	}

	// Step 4: prepare snapshot config files dir
	if err = prepareSnapshotFiles(conf); err != nil {
		return nil, fmt.Errorf("prepare vm snapshot files failed: %v", err)
	}

	return conf, nil
}

// AcquireView views and mounts 3 committed snapshots for snapshot-based startup.
// If already viewed and mounted, reuses existing mounts (refCount++).
func (s *server) AcquireView(
	ctx context.Context,
	namespace, key string,
	parents ParentSnapshotIDs,
	opts ...Opt,
) (_ *SnapshotConfig, err error) {
	rootfsViewAliasKey := getRootfsViewAliasKey(key)
	rootfsViewSnapshotKey := getSharedViewSnapshotKey(common.SnapshotMountRootfs, parents.Rootfs)
	memViewAliasKey := getMemViewAliasKey(key)
	memViewSnapshotKey := getSharedViewSnapshotKey(common.SnapshotMountMem, parents.Mem)
	vmViewAliasKey := getVMViewAliasKey(key)
	vmViewSnapshotKey := getSharedViewSnapshotKey(common.SnapshotMountVM, parents.VM)

	conf := &SnapshotConfig{
		Rootfs: getSharedMountPath(s.workDir, namespace, parents.Rootfs),
		MemDir: getSharedMountPath(s.workDir, namespace, parents.Mem),
		VmDir:  getSharedMountPath(s.workDir, namespace, parents.VM),
	}
	conf.initDefaults()
	for _, o := range opts {
		o(conf)
	}
	conf.createLabels()

	ops := &snapshotOps{server: s}

	var cleanups []*snapshotCleaner
	defer func() {
		if err != nil {
			for _, c := range cleanups {
				if c != nil {
					c.Cleanup()
				}
			}
		}
	}()

	// Step 1: view rootfs
	rootfsCleaner, err := ops.viewSnapshot(ctx, namespace, parents.Rootfs, rootfsViewAliasKey, rootfsViewSnapshotKey, conf.Rootfs, withLabels(conf))
	if err != nil {
		return nil, fmt.Errorf("view rootfs failed: %v", err)
	}
	cleanups = append(cleanups, rootfsCleaner)

	conf.pmemFiles, err = listRootfsLayerErofs(conf.Rootfs)
	if err != nil {
		return nil, fmt.Errorf("list rootfs layer erofs failed: %v", err)
	}

	// Step 2: view vm
	vmCleaner, err := ops.viewSnapshot(ctx, namespace, parents.VM, vmViewAliasKey, vmViewSnapshotKey, conf.VmDir)
	if err != nil {
		return nil, fmt.Errorf("view vm failed: %v", err)
	}
	cleanups = append(cleanups, vmCleaner)

	// Step 3: view mem + verify mem.img
	memCleaner, err := ops.viewSnapshot(ctx, namespace, parents.Mem, memViewAliasKey, memViewSnapshotKey, conf.MemDir)
	if err != nil {
		return nil, fmt.Errorf("view mem failed: %v", err)
	}
	cleanups = append(cleanups, memCleaner)

	if err = ensureMemFile(conf, conf.MemDir, false); err != nil {
		return nil, fmt.Errorf("mem.img verification failed: %v", err)
	}

	return conf, nil
}

// AcquireResumeWorkspace prepares a restore workspace for snapshot-based startup.
// Rootfs and VM remain shared views, while mem is prepared as an active layer so
// the snapshot config can be rewritten before restore.
func (s *server) AcquireResumeWorkspace(
	ctx context.Context,
	namespace, key string,
	parents ParentSnapshotIDs,
	opts ...Opt,
) (_ *SnapshotConfig, err error) {
	memKey := getMemKeyFromRootfs(key)
	rootfsViewAliasKey := getRootfsViewAliasKey(key)
	rootfsViewSnapshotKey := getSharedViewSnapshotKey(common.SnapshotMountRootfs, parents.Rootfs)
	vmViewAliasKey := getVMViewAliasKey(key)
	vmViewSnapshotKey := getSharedViewSnapshotKey(common.SnapshotMountVM, parents.VM)

	conf := &SnapshotConfig{
		Rootfs: getSharedMountPath(s.workDir, namespace, parents.Rootfs),
		MemDir: getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountMem),
		VmDir:  getSharedMountPath(s.workDir, namespace, parents.VM),
	}
	conf.initDefaults()
	for _, o := range opts {
		o(conf)
	}
	conf.createLabels()

	ops := &snapshotOps{server: s}

	type cleanupItem struct {
		key     string
		cleaner *snapshotCleaner
	}
	var activeCleanups []cleanupItem
	var viewCleanups []*snapshotCleaner
	defer func() {
		if err != nil {
			for _, item := range activeCleanups {
				s.removeActiveSnapshot(namespace, item.key)
				if item.cleaner != nil {
					item.cleaner.Cleanup()
				}
			}
			for _, cleaner := range viewCleanups {
				if cleaner != nil {
					cleaner.Cleanup()
				}
			}
		}
	}()

	rootfsCleaner, err := ops.viewSnapshot(ctx, namespace, parents.Rootfs, rootfsViewAliasKey, rootfsViewSnapshotKey, conf.Rootfs, withLabels(conf))
	if err != nil {
		return nil, fmt.Errorf("view rootfs failed: %v", err)
	}
	viewCleanups = append(viewCleanups, rootfsCleaner)

	conf.pmemFiles, err = listRootfsLayerErofs(conf.Rootfs)
	if err != nil {
		return nil, fmt.Errorf("list rootfs layer erofs failed: %v", err)
	}

	vmCleaner, err := ops.viewSnapshot(ctx, namespace, parents.VM, vmViewAliasKey, vmViewSnapshotKey, conf.VmDir)
	if err != nil {
		return nil, fmt.Errorf("view vm failed: %v", err)
	}
	viewCleanups = append(viewCleanups, vmCleaner)

	memCleaner, err := ops.prepareAndRegisterSnapshot(ctx, NewSnapshotLocator(namespace, memKey, parents.Mem), conf.MemDir)
	if err != nil {
		return nil, err
	}
	activeCleanups = append(activeCleanups, cleanupItem{key: memKey, cleaner: memCleaner})

	if err = ensureMemFile(conf, conf.MemDir, false); err != nil {
		return nil, fmt.Errorf("mem.img verification failed: %v", err)
	}

	configUpdater := &configUpdater{}
	configFilePath := filepath.Join(conf.SnapDir(), common.SnapshotConfigFileName)
	if err = configUpdater.updateSnapshotConfig(
		configFilePath,
		conf.KernelFile(),
		conf.InitrdFile(),
		conf.SnapshotMemFile(),
		conf.PmemFiles(),
	); err != nil {
		return nil, fmt.Errorf("update snapshot config failed: %v", err)
	}

	return conf, nil
}

// ResolveParentSnapshotIDs resolves parent mem/vm snapshots from rootfs snapshot.
func (s *server) ResolveParentSnapshotIDs(namespace, rootfs string) (ParentSnapshotIDs, error) {
	if rootfs == "" {
		return ParentSnapshotIDs{}, nil
	}
	if _, err := s.snt.Stat(context.Background(), namespace, rootfs); err != nil {
		return ParentSnapshotIDs{}, fmt.Errorf("rootfs snapshot %s not found (maybe not unpacked): %v", rootfs, err)
	}
	parentMem, err := s.getRelatedSnapshotID(namespace, rootfs, common.SnapshotLabelMemSnapshot)
	if err != nil {
		return ParentSnapshotIDs{}, err
	}
	parentVM, err := s.getRelatedSnapshotID(namespace, rootfs, common.SnapshotLabelVMSnapshot)
	if err != nil {
		return ParentSnapshotIDs{}, err
	}
	return ParentSnapshotIDs{
		Rootfs: rootfs,
		Mem:    parentMem,
		VM:     parentVM,
	}, nil
}

// Commit commits an active snapshot with externally calculated snapshotID.
func (s *server) Commit(ctx context.Context, namespace, snapshotID, key string, opts ...Opt) error {
	si := s.getSnapshot(namespace, key)
	if si == nil {
		return fmt.Errorf("snapshot [%s:%s] not found", namespace, key)
	}

	memKey := getMemKeyFromRootfs(key)
	memInfo := s.getSnapshot(namespace, memKey)
	if memInfo == nil {
		return fmt.Errorf("mem snapshot [%s:%s] not found", namespace, memKey)
	}
	vmViewAliasKey := getVMViewAliasKey(key)
	parentVMSnapshotID, ok := s.viewMgr.getViewAlias(namespace, vmViewAliasKey)
	if !ok {
		return fmt.Errorf("vm view alias [%s:%s] not found", namespace, vmViewAliasKey)
	}

	memSnapshotID, err := CalculateSnapshotID(namespace, memKey, "")
	if err != nil {
		return fmt.Errorf("calculate mem snapshot id failed: %v", err)
	}

	if snapshotID == "" {
		return fmt.Errorf("rootfs snapshot id is required (compute externally)")
	}

	ops := &snapshotOps{server: s}
	conf, viewConf, err := ops.buildCommitConfigs(ctx, namespace, key, memKey, snapshotID, memSnapshotID, parentVMSnapshotID, si, opts)
	if err != nil {
		return err
	}

	configUpdater := &configUpdater{}
	configFilePath := filepath.Join(conf.SnapDir(), common.SnapshotConfigFileName)
	if err := configUpdater.updateSnapshotConfig(configFilePath, viewConf.KernelFile(), viewConf.InitrdFile(), viewConf.SnapshotMemFile(), viewConf.PmemFiles()); err != nil {
		return fmt.Errorf("update snapshot config failed: %v", err)
	}

	if err := ops.commitRootfsSnapshot(ctx, namespace, key, snapshotID, conf, memSnapshotID, parentVMSnapshotID); err != nil {
		return err
	}

	if err := ops.commitMemSnapshot(ctx, namespace, memKey, memSnapshotID, snapshotID); err != nil {
		return err
	}
	if err := ops.prewarmViewMounts(ctx, namespace, snapshotID, parentVMSnapshotID, viewConf); err != nil {
		return err
	}

	return nil
}

// Remove removes snapshots and cleans up associated resources.
func (s *server) Remove(ctx context.Context, namespace, key string) error {
	memKey := getMemKeyFromRootfs(key)
	rootfsViewAliasKey := getRootfsViewAliasKey(key)
	memViewAliasKey := getMemViewAliasKey(key)
	vmViewAliasKey := getVMViewAliasKey(key)

	type activeItem struct {
		key  string
		info *snapshots.Info
	}
	var activeItems []activeItem
	var viewKeys []string
	var missingKeys []string
	var errs []error

	if info := s.getSnapshot(namespace, key); info != nil {
		activeItems = append(activeItems, activeItem{key: key, info: info})
	} else if _, ok := s.viewMgr.getViewAlias(namespace, rootfsViewAliasKey); ok {
		viewKeys = append(viewKeys, rootfsViewAliasKey)
	} else {
		missingKeys = append(missingKeys, key)
	}

	if info := s.getSnapshot(namespace, memKey); info != nil {
		activeItems = append(activeItems, activeItem{key: memKey, info: info})
	} else if _, ok := s.viewMgr.getViewAlias(namespace, memViewAliasKey); ok {
		viewKeys = append(viewKeys, memViewAliasKey)
	} else {
		missingKeys = append(missingKeys, memKey)
	}

	if _, ok := s.viewMgr.getViewAlias(namespace, vmViewAliasKey); ok {
		viewKeys = append(viewKeys, vmViewAliasKey)
	} else {
		missingKeys = append(missingKeys, vmViewAliasKey)
	}

	if len(activeItems) == 0 && len(viewKeys) == 0 {
		return fmt.Errorf("snapshots [%s:%s,%s] and view aliases [%s,%s,%s] not found in active/view caches", namespace, key, memKey, rootfsViewAliasKey, memViewAliasKey, vmViewAliasKey)
	}

	var unmountErrs []error
	ops := &snapshotOps{server: s}
	for _, item := range activeItems {
		mountPoint := ""
		if item.key == key {
			mountPoint = getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountRootfs)
			if rootfs, ok := item.info.Labels[common.SnapshotLabelRootfs]; ok && rootfs != "" {
				mountPoint = rootfs
			}
		} else if item.key == memKey {
			mountPoint = getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountMem)
		}
		if err := ops.unmountPath(mountPoint); err != nil {
			unmountErrs = append(unmountErrs, err)
		}
	}

	// If unmount failed, don't remove/release to avoid inconsistent state.
	if len(unmountErrs) > 0 {
		return fmt.Errorf("unmount failed, skip cleanup to avoid orphaned dirs: %w", errors.Join(unmountErrs...))
	}

	if len(viewKeys) > 0 {
		if _, releaseErr := s.viewMgr.releaseViewAliases(s.snt, namespace, viewKeys...); releaseErr != nil {
			errs = append(errs, releaseErr)
		}
	}

	for _, item := range activeItems {
		if err := ops.tryRemoveSnapshot(ctx, namespace, item.key); err != nil {
			errs = append(errs, err)
		}
	}
	if len(missingKeys) > 0 {
		errs = append(errs, fmt.Errorf("some snapshot keys not found in active/view caches: %v", missingKeys))
	}

	return errors.Join(errs...)
}

// CleanupAllViews unmounts and removes all view snapshots.
// Should be called during graceful shutdown before Close().
func (s *server) CleanupAllViews() {
	s.viewMgr.CleanupAllViews(s.snt)
}

// Close releases snapshot resources.
func (s *server) Close() error {
	return nil
}

// withLabels creates a snapshot option with labels from config.
func withLabels(conf *SnapshotConfig) snapshots.Opt {
	return func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for k, v := range conf.Labels {
			info.Labels[k] = v
		}
		return nil
	}
}
