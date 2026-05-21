package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/snapshot/snapshotter"
)

// server manages snapshot lifecycle with caching and view sharing.
type server struct {
	snt              snapshotter.Snapshotter
	rootfsSnt        snapshotter.Snapshotter
	mountMgr         mount.Manager
	activeSnapshots  map[string]map[string]*snapshots.Info
	activeRootfsPmem map[string]map[string][]string
	lock             sync.RWMutex
	workDir          string
	viewMgr          *viewManager
}

var gServer server

// NewServer initializes the snapshot server with containerd client.
func NewServer(workDir string, daemonClient *daemon.Client) error {
	if daemonClient == nil {
		return fmt.Errorf("containerd client is nil")
	}
	erofsSn, err := snapshotter.NewContainerdSnap(
		daemonClient.SnapshotService("erofs"),
		daemonClient.NamespaceService(),
		daemonClient.DefaultNamespace(),
		snapshotter.WithNamespaceContext(daemonClient.WithNamespace),
	)
	if err != nil {
		return err
	}
	gServer.snt = erofsSn
	gServer.rootfsSnt = erofsSn
	gServer.mountMgr = daemonClient.MountManager()
	gServer.workDir = workDir
	gServer.activeSnapshots = make(map[string]map[string]*snapshots.Info)
	gServer.activeRootfsPmem = make(map[string]map[string][]string)
	gServer.viewMgr = &viewManager{
		viewMounts:  make(map[string]map[string]*viewMountRef),
		viewAliases: make(map[string]map[string]string),
	}

	return nil
}

// getActiveSnapshot retrieves runtime active snapshot info from cache.
func (s *server) getActiveSnapshot(ns, key string) *snapshots.Info {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if m, ok := s.activeSnapshots[ns]; ok {
		return m[key]
	}
	return nil
}

// addActiveSnapshot adds active snapshot info to the runtime cache.
func (s *server) addActiveSnapshot(ns, key string, info *snapshots.Info) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if m, ok := s.activeSnapshots[ns]; ok {
		m[key] = info
	} else {
		m := make(map[string]*snapshots.Info)
		m[key] = info
		s.activeSnapshots[ns] = m
	}
}

// removeActiveSnapshot removes active snapshot info from the runtime cache.
func (s *server) removeActiveSnapshot(ns, key string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if m, ok := s.activeSnapshots[ns]; ok {
		delete(m, key)
	}
	if m, ok := s.activeRootfsPmem[ns]; ok {
		delete(m, key)
	}
}

func (s *server) addActiveRootfsPmem(ns, key string, files []string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.activeRootfsPmem == nil {
		s.activeRootfsPmem = make(map[string]map[string][]string)
	}
	if _, ok := s.activeRootfsPmem[ns]; !ok {
		s.activeRootfsPmem[ns] = make(map[string][]string)
	}
	s.activeRootfsPmem[ns][key] = append([]string(nil), files...)
}

func (s *server) getActiveRootfsPmem(ns, key string) []string {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if m, ok := s.activeRootfsPmem[ns]; ok {
		if files, ok := m[key]; ok {
			return append([]string(nil), files...)
		}
	}
	return nil
}

func (s *server) removeRootfsSnapshot(namespace, key string) {
	if s.rootfsSnt != nil {
		_ = s.rootfsSnt.Remove(context.Background(), namespace, key)
	}
	s.removeActiveSnapshot(namespace, key)
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
	if err := mount.UnmountAll(path, unix.MNT_FORCE); err != nil {
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

// Prepare creates active snapshots for rootfs/mem and a shared view for vm.
func (s *server) Prepare(
	ctx context.Context,
	namespace, key string,
	parents ParentSnapshotIDs,
	opts ...Opt,
) (_ *SnapshotConfig, err error) {
	if si := s.getActiveSnapshot(namespace, key); si != nil {
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
			if parents.Rootfs != "" {
				s.removeRootfsSnapshot(namespace, key)
			}
			for _, item := range activeCleanups {
				s.removeActiveSnapshot(namespace, item.key)
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
	conf.pmemFiles, err = ops.prepareRootfsSnapshot(
		ctx,
		NewSnapshotLocator(namespace, key, parents.Rootfs),
		withLabels(conf),
	)
	if err != nil {
		return nil, err
	}
	s.addActiveRootfsPmem(namespace, key, conf.pmemFiles)

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
	conf.MemDir = memCleaner.accessPath

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

	// Step 1: view rootfs and resolve EROFS PMEM files
	rootfsCleaner, pmemFiles, err := ops.viewRootfsSnapshot(ctx, namespace, parents.Rootfs, rootfsViewAliasKey, rootfsViewSnapshotKey, conf.Rootfs)
	if err != nil {
		return nil, fmt.Errorf("resolve rootfs erofs pmem files failed: %v", err)
	}
	cleanups = append(cleanups, rootfsCleaner)
	conf.pmemFiles = pmemFiles

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
	cid uint32,
	socketPath string,
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

	rootfsCleaner, pmemFiles, err := ops.viewRootfsSnapshot(ctx, namespace, parents.Rootfs, rootfsViewAliasKey, rootfsViewSnapshotKey, conf.Rootfs)
	if err != nil {
		return nil, fmt.Errorf("resolve rootfs erofs pmem files failed: %v", err)
	}
	viewCleanups = append(viewCleanups, rootfsCleaner)
	conf.pmemFiles = pmemFiles

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
	conf.MemDir = memCleaner.accessPath

	if err = ensureMemFile(conf, conf.MemDir, false); err != nil {
		return nil, fmt.Errorf("mem.img verification failed: %v", err)
	}

	configUpdater := &configUpdater{}
	configFilePath := filepath.Join(conf.SnapDir(), common.SnapshotConfigFileName)
	pmemDeviceCount, err := readSnapshotPmemDeviceCount(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot pmem device count failed: %v", err)
	}
	conf.pmemFiles, err = selectSnapshotRestorePmemFiles(conf.pmemFiles, pmemDeviceCount)
	if err != nil {
		return nil, fmt.Errorf("select restore rootfs pmem files failed: %v", err)
	}
	if err = configUpdater.updateSnapshotConfig(
		configFilePath,
		conf.KernelFile(),
		conf.InitrdFile(),
		conf.SnapshotMemFile(),
		conf.PmemFiles(),
		cid,
		socketPath,
	); err != nil {
		return nil, fmt.Errorf("update snapshot config failed: %v", err)
	}

	return conf, nil
}

func (s *server) resolveParentSnapshotIDs(namespace, rootfs string, allowEmptyMem bool) (ParentSnapshotIDs, error) {
	if rootfs == "" {
		return ParentSnapshotIDs{}, nil
	}
	info, err := s.rootfsSnt.Stat(context.Background(), namespace, rootfs)
	if err != nil {
		return ParentSnapshotIDs{}, fmt.Errorf("rootfs snapshot %s not found (maybe not unpacked): %v", rootfs, err)
	}
	parentMem, ok := info.Labels[common.SnapshotLabelMemSnapshot]
	if (!ok || parentMem == "") && !allowEmptyMem {
		return ParentSnapshotIDs{}, fmt.Errorf("mem snapshot label not found on rootfs snapshot %s", rootfs)
	}
	parentVM, ok := info.Labels[common.SnapshotLabelVMSnapshot]
	if !ok || parentVM == "" {
		return ParentSnapshotIDs{}, fmt.Errorf("vm snapshot label not found on rootfs snapshot %s", rootfs)
	}
	return ParentSnapshotIDs{
		Rootfs: rootfs,
		Mem:    parentMem,
		VM:     parentVM,
	}, nil
}

// ResolveParentSnapshotIDs resolves parent mem/vm snapshots from a committed rootfs snapshot.
// This is the strict path used for snapshot-based startup and requires both mem/vm labels.
func (s *server) ResolveParentSnapshotIDs(namespace, rootfs string) (ParentSnapshotIDs, error) {
	return s.resolveParentSnapshotIDs(namespace, rootfs, false)
}

// ResolveImageParentSnapshotIDs resolves image startup parents from a rootfs snapshot.
// For image startup, mem snapshot label is optional and an empty mem parent means
// a fresh writable mem layer will be prepared for the sandbox.
func (s *server) ResolveImageParentSnapshotIDs(namespace, rootfs string) (ParentSnapshotIDs, error) {
	return s.resolveParentSnapshotIDs(namespace, rootfs, true)
}

// Commit commits an active snapshot with externally calculated snapshotID.
func (s *server) Commit(ctx context.Context, namespace, snapshotID, key string, opts ...Opt) (string, error) {
	si := s.getActiveSnapshot(namespace, key)
	if si == nil {
		return "", fmt.Errorf("snapshot [%s:%s] not found", namespace, key)
	}

	memKey := getMemKeyFromRootfs(key)
	memInfo := s.getActiveSnapshot(namespace, memKey)
	if memInfo == nil {
		return "", fmt.Errorf("mem snapshot [%s:%s] not found", namespace, memKey)
	}
	vmViewAliasKey := getVMViewAliasKey(key)
	parentVMSnapshotID, ok := s.viewMgr.getViewAlias(namespace, vmViewAliasKey)
	if !ok {
		return "", fmt.Errorf("vm view alias [%s:%s] not found", namespace, vmViewAliasKey)
	}

	memSnapshotID, err := CalculateSnapshotID(namespace, memKey, "")
	if err != nil {
		return "", fmt.Errorf("calculate mem snapshot id failed: %v", err)
	}

	if snapshotID == "" {
		return "", fmt.Errorf("rootfs snapshot id is required (compute externally)")
	}

	ops := &snapshotOps{server: s}
	conf, viewConf, err := ops.buildCommitConfigs(ctx, namespace, key, memKey, snapshotID, memSnapshotID, parentVMSnapshotID, si, opts)
	if err != nil {
		return "", err
	}

	configUpdater := &configUpdater{}
	configFilePath := filepath.Join(conf.SnapDir(), common.SnapshotConfigFileName)
	if err := configUpdater.updateSnapshotConfig(configFilePath, viewConf.KernelFile(), viewConf.InitrdFile(), viewConf.SnapshotMemFile(), viewConf.PmemFiles(), 0, ""); err != nil {
		return "", fmt.Errorf("update snapshot config failed: %v", err)
	}

	finalRootfsSnapshotID, err := ops.commitRootfsSnapshot(ctx, namespace, key, snapshotID, conf, memSnapshotID, parentVMSnapshotID, si)
	if err != nil {
		return "", err
	}

	if err := ops.commitMemSnapshot(ctx, namespace, memKey, memSnapshotID, finalRootfsSnapshotID); err != nil {
		return "", err
	}
	if err := ops.prewarmViewMounts(ctx, namespace, finalRootfsSnapshotID, parentVMSnapshotID, viewConf); err != nil {
		return "", err
	}

	return finalRootfsSnapshotID, nil
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

	if info := s.getActiveSnapshot(namespace, key); info != nil {
		activeItems = append(activeItems, activeItem{key: key, info: info})
	} else if _, ok := s.viewMgr.getViewAlias(namespace, rootfsViewAliasKey); ok {
		viewKeys = append(viewKeys, rootfsViewAliasKey)
	} else {
		missingKeys = append(missingKeys, key)
	}

	if info := s.getActiveSnapshot(namespace, memKey); info != nil {
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
		if s.mountMgr != nil {
			activationKey := mountActivationKey("active", namespace, item.key)
			if err := s.mountMgr.Deactivate(namespaces.WithNamespace(ctx, namespace), activationKey); err != nil && !errdefs.IsNotFound(err) {
				unmountErrs = append(unmountErrs, fmt.Errorf("deactivate mount %s: %w", activationKey, err))
			}
		}
	}

	// If unmount failed, don't remove/release to avoid inconsistent state.
	if len(unmountErrs) > 0 {
		return fmt.Errorf("unmount failed, skip cleanup to avoid orphaned dirs: %w", errors.Join(unmountErrs...))
	}

	if len(viewKeys) > 0 {
		if _, releaseErr := s.viewMgr.releaseViewAliases(s.snt, s.mountMgr, namespace, viewKeys...); releaseErr != nil {
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
	s.viewMgr.CleanupAllViews(s.snt, s.mountMgr)
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
