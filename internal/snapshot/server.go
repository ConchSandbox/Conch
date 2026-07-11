package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/snapshot/snapshotter"
)

// Server manages snapshot lifecycle and per-sandbox boot layouts.
type Server struct {
	snt              snapshotter.Snapshotter
	mountMgr         mount.Manager
	activeSnapshots  map[runtimeSnapshotKey]*snapshots.Info
	activeRootfsPmem map[runtimeSnapshotKey][]string
	lock             sync.RWMutex
	workDir          string
}

type runtimeSnapshotKey struct {
	namespace string
	key       string
}

type BootLayout struct {
	RootfsMount string // dir which mounts rootfs snapshot view
	MemMount    string // dir which mounts mem snapshot view or active layer
	VMMount     string // dir which mounts sandbox snapshot view

	SnapshotDir  string // dir which stores vm snapshot, relative to MemMount
	MemorySizeMB int64  // memory size of vm, unit is mb

	pmemFiles []string // pmem array (e.g. layer1.erofs, layer2.erofs, layer3.erofs)
}

func (w *BootLayout) PmemFiles() []string {
	result := make([]string, 0, len(w.pmemFiles))
	for _, name := range w.pmemFiles {
		if filepath.IsAbs(name) {
			result = append(result, name)
			continue
		}
		result = append(result, filepath.Join(w.RootfsMount, name))
	}
	return result
}

func (w *BootLayout) SnapshotMemFile() string {
	return filepath.Join(w.MemMount, common.MemFileName)
}

func (w *BootLayout) InitrdFile() string {
	return filepath.Join(w.VMMount, common.VmInitrdRelativePath)
}

func (w *BootLayout) KernelFile() string {
	return filepath.Join(w.VMMount, common.VmKernelRelativePath)
}

func (w *BootLayout) SnapDir() string {
	return filepath.Join(w.MemMount, strings.TrimLeft(w.SnapshotDir, string(filepath.Separator)))
}

// initDefaults sets default values for BootLayout fields.
func (w *BootLayout) initDefaults() {
	if w.MemorySizeMB <= 0 {
		w.MemorySizeMB = common.MemFileDefaultSize
	}
	if w.SnapshotDir == "" {
		w.SnapshotDir = "conch/snapshot"
	}
	if w.pmemFiles == nil {
		w.pmemFiles = make([]string, 0)
	}
}

// ParentSnapshotIDs groups parent snapshot IDs for image-based startup.
type ParentSnapshotIDs struct {
	Rootfs string
	Mem    string
	VM     string
}

func getSnapshotBasePath(workDir, namespace string) string {
	return filepath.Join(workDir, "snapshot", namespace)
}

func snapshotPathName(snapshotID string) string {
	return strings.ReplaceAll(snapshotID, ":", "")
}

func getActiveMountPath(workDir, namespace, sandboxID, mountKind string) string {
	return filepath.Join(getSnapshotBasePath(workDir, namespace), sandboxID, mountKind)
}

func getMemKeyFromRootfs(rootfsKey string) string {
	return rootfsKey + common.MemKeySuffix
}

func getRootfsViewSnapshotKey(sandboxID string) string {
	return fmt.Sprintf("view-%s-%s", common.SnapshotMountRootfs, sandboxID)
}

func getVMViewSnapshotKey(sandboxID string) string {
	return fmt.Sprintf("view-%s-%s", common.SnapshotMountVM, sandboxID)
}

// NewServer initializes the snapshot server with containerd client.
func NewServer(workDir string, daemonClient *containerdclient.Client) (*Server, error) {
	if daemonClient == nil {
		return nil, fmt.Errorf("containerd client is nil")
	}
	erofsSn, err := snapshotter.NewContainerdSnap(
		daemonClient.SnapshotService("erofs"),
		daemonClient.NamespaceService(),
		daemonClient.DefaultNamespace(),
		snapshotter.WithNamespaceContext(daemonClient.WithNamespace),
	)
	if err != nil {
		return nil, err
	}
	srv := &Server{
		snt:              erofsSn,
		mountMgr:         daemonClient.MountManager(),
		workDir:          workDir,
		activeSnapshots:  make(map[runtimeSnapshotKey]*snapshots.Info),
		activeRootfsPmem: make(map[runtimeSnapshotKey][]string),
	}
	if srv.snt == nil {
		return nil, fmt.Errorf("snapshot server snapshotter is nil")
	}
	return srv, nil
}

// getActiveSnapshot retrieves runtime active snapshot info from cache.
func (s *Server) getActiveSnapshot(ns, key string) *snapshots.Info {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.activeSnapshots[runtimeSnapshotKey{namespace: ns, key: key}]
}

// addActiveSnapshot adds active snapshot info to the runtime cache.
func (s *Server) addActiveSnapshot(ns, key string, info *snapshots.Info) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.activeSnapshots == nil {
		s.activeSnapshots = make(map[runtimeSnapshotKey]*snapshots.Info)
	}
	s.activeSnapshots[runtimeSnapshotKey{namespace: ns, key: key}] = info
}

// removeActiveSnapshot removes active snapshot info from the runtime cache.
func (s *Server) removeActiveSnapshot(ns, key string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	activeKey := runtimeSnapshotKey{namespace: ns, key: key}
	delete(s.activeSnapshots, activeKey)
	delete(s.activeRootfsPmem, activeKey)
}

func (s *Server) addActiveRootfsPmem(ns, key string, files []string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.activeRootfsPmem == nil {
		s.activeRootfsPmem = make(map[runtimeSnapshotKey][]string)
	}
	s.activeRootfsPmem[runtimeSnapshotKey{namespace: ns, key: key}] = append([]string(nil), files...)
}

func (s *Server) getActiveRootfsPmem(ns, key string) []string {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if files, ok := s.activeRootfsPmem[runtimeSnapshotKey{namespace: ns, key: key}]; ok {
		return append([]string(nil), files...)
	}
	return nil
}

func (s *Server) removeRootfsSnapshot(namespace, key string) {
	if s.snt != nil {
		_ = s.snt.Remove(context.Background(), namespace, key)
	}
	s.removeActiveSnapshot(namespace, key)
}

// unmountPath unmounts a filesystem path and removes the directory.
// Skips if path doesn't exist (may have been cleaned up already).
func (s *Server) unmountPath(path string) error {
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

// CreateBootLayout creates active rootfs/mem snapshots and a per-sandbox VM view for template startup.
func (s *Server) CreateBootLayout(
	ctx context.Context,
	namespace, key string,
	parents ParentSnapshotIDs,
	memorySizeMB int64,
) (_ *BootLayout, err error) {
	if si := s.getActiveSnapshot(namespace, key); si != nil {
		return nil, fmt.Errorf("snapshot [%s:%s] existed", namespace, key)
	}

	memKey := getMemKeyFromRootfs(key)
	vmViewSnapshotKey := getVMViewSnapshotKey(key)

	layout := &BootLayout{
		RootfsMount: getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountRootfs),
		MemMount:    getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountMem),
		VMMount:     getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountVM),
	}
	layout.initDefaults()
	if memorySizeMB > 0 {
		layout.MemorySizeMB = memorySizeMB
	}
	labels := bootLayoutLabels(layout, nil)

	// Step 1: prepare rootfs
	err = s.prepareRootfsSnapshot(
		ctx,
		namespace,
		key,
		parents.Rootfs,
		layout,
		withLabels(labels),
	)
	if err != nil {
		return nil, err
	}
	if parents.Rootfs != "" {
		defer func() {
			if err != nil {
				s.removeRootfsSnapshot(namespace, key)
			}
		}()
	}

	// Step 2: view vm (read-only, per-sandbox)
	if _, err = s.viewSnapshotMount(ctx, namespace, parents.VM, vmViewSnapshotKey, layout.VMMount); err != nil {
		return nil, fmt.Errorf("view vm failed: %v", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if releaseErr := s.releaseViewSnapshot(ctx, namespace, vmViewSnapshotKey, layout.VMMount); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()

	// Step 3: prepare mem + create sparse memfile
	memMountPoint := layout.MemMount
	memAccessPath, err := s.prepareAndMountActiveSnapshot(ctx, namespace, memKey, parents.Mem, memMountPoint)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err == nil {
			return
		}
		s.removeActiveSnapshot(namespace, memKey)
		if unmountErr := s.unmountPath(memMountPoint); unmountErr != nil {
			err = errors.Join(err, unmountErr)
		}
		if s.mountMgr != nil {
			activationKey := mountActivationKey("active", namespace, memKey)
			if deactivateErr := s.mountMgr.Deactivate(namespaces.WithNamespace(ctx, namespace), activationKey); deactivateErr != nil && !errdefs.IsNotFound(deactivateErr) {
				err = errors.Join(err, fmt.Errorf("deactivate mount %s: %w", activationKey, deactivateErr))
			}
		}
		if removeErr := s.tryRemoveSnapshot(ctx, namespace, memKey); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
	}()
	layout.MemMount = memAccessPath

	if err = ensureMemFile(layout, layout.MemMount, true); err != nil {
		return nil, fmt.Errorf("prepare mem.img failed: %v", err)
	}

	// Step 4: prepare snapshot config files dir
	if err = prepareSnapshotFiles(layout); err != nil {
		return nil, fmt.Errorf("prepare vm snapshot files failed: %v", err)
	}

	return layout, nil
}

// RestoreBootLayout creates a restore layout for checkpoint startup.
// Rootfs and VM are per-sandbox views, while mem is prepared as an active layer
// so the snapshot config can be rewritten before restore.
func (s *Server) RestoreBootLayout(
	ctx context.Context,
	namespace, key string,
	parents ParentSnapshotIDs,
	cid uint32,
	socketPath string,
) (_ *BootLayout, err error) {
	memKey := getMemKeyFromRootfs(key)
	rootfsViewSnapshotKey := getRootfsViewSnapshotKey(key)
	vmViewSnapshotKey := getVMViewSnapshotKey(key)

	layout := &BootLayout{
		RootfsMount: getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountRootfs),
		MemMount:    getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountMem),
		VMMount:     getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountVM),
	}
	layout.initDefaults()
	memorySizeFromSnapshot, err := s.loadCommittedBootLayoutMetadata(ctx, namespace, parents, layout)
	if err != nil {
		return nil, err
	}

	pmemFiles, err := s.viewRootfsSnapshot(ctx, namespace, parents.Rootfs, rootfsViewSnapshotKey, layout.RootfsMount)
	if err != nil {
		return nil, fmt.Errorf("resolve rootfs erofs pmem files failed: %v", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if releaseErr := s.releaseViewSnapshot(ctx, namespace, rootfsViewSnapshotKey, layout.RootfsMount); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()
	layout.pmemFiles = pmemFiles

	if _, err := s.viewSnapshotMount(ctx, namespace, parents.VM, vmViewSnapshotKey, layout.VMMount); err != nil {
		return nil, fmt.Errorf("view vm failed: %v", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if releaseErr := s.releaseViewSnapshot(ctx, namespace, vmViewSnapshotKey, layout.VMMount); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()

	memMountPoint := layout.MemMount
	memAccessPath, err := s.prepareAndMountActiveSnapshot(ctx, namespace, memKey, parents.Mem, memMountPoint)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err == nil {
			return
		}
		s.removeActiveSnapshot(namespace, memKey)
		if unmountErr := s.unmountPath(memMountPoint); unmountErr != nil {
			err = errors.Join(err, unmountErr)
		}
		if s.mountMgr != nil {
			activationKey := mountActivationKey("active", namespace, memKey)
			if deactivateErr := s.mountMgr.Deactivate(namespaces.WithNamespace(ctx, namespace), activationKey); deactivateErr != nil && !errdefs.IsNotFound(deactivateErr) {
				err = errors.Join(err, fmt.Errorf("deactivate mount %s: %w", activationKey, deactivateErr))
			}
		}
		if removeErr := s.tryRemoveSnapshot(ctx, namespace, memKey); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
	}()
	layout.MemMount = memAccessPath

	if err = ensureMemFile(layout, layout.MemMount, false); err != nil {
		return nil, fmt.Errorf("mem.img verification failed: %v", err)
	}
	if !memorySizeFromSnapshot {
		info, statErr := os.Stat(layout.SnapshotMemFile())
		if statErr != nil {
			err = statErr
			return nil, fmt.Errorf("resolve mem.img size failed: %v", err)
		}
		if size := info.Size(); size > 0 {
			memorySizeMB := size / common.MemMB
			if size%common.MemMB != 0 {
				memorySizeMB++
			}
			if memorySizeMB > 0 {
				layout.MemorySizeMB = memorySizeMB
			}
		}
	}

	configUpdater := &configUpdater{}
	configFilePath := filepath.Join(layout.SnapDir(), common.SnapshotConfigFileName)
	pmemDeviceCount, err := readSnapshotPmemDeviceCount(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot pmem device count failed: %v", err)
	}
	layout.pmemFiles, err = selectSnapshotRestorePmemFiles(layout.pmemFiles, pmemDeviceCount)
	if err != nil {
		return nil, fmt.Errorf("select restore rootfs pmem files failed: %v", err)
	}
	if err = configUpdater.updateSnapshotConfig(
		configFilePath,
		layout.KernelFile(),
		layout.InitrdFile(),
		layout.SnapshotMemFile(),
		cid,
		socketPath,
	); err != nil {
		return nil, fmt.Errorf("update snapshot config failed: %v", err)
	}

	return layout, nil
}

// CommitBootLayout commits a captured VMM snapshot without consuming or
// mutating the sandbox's live active rootfs/mem layers.
func (s *Server) CommitBootLayout(ctx context.Context, namespace, snapshotID, key, capturePath, parentVMID string) (string, error) {
	return s.commitCheckpointBootLayout(ctx, namespace, snapshotID, key, capturePath, parentVMID)
}

// ReleaseBootLayout releases active snapshots and per-sandbox views for a runtime layout.
func (s *Server) ReleaseBootLayout(ctx context.Context, namespace, key string) error {
	memKey := getMemKeyFromRootfs(key)
	var errs []error

	rootfsMount := getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountRootfs)
	if s.activeSnapshotExists(ctx, namespace, key) {
		if err := s.releaseActiveSnapshot(ctx, namespace, key, rootfsMount); err != nil {
			errs = append(errs, err)
		}
	} else if err := s.releaseViewSnapshot(ctx, namespace, getRootfsViewSnapshotKey(key), rootfsMount); err != nil {
		errs = append(errs, err)
	}

	if err := s.releaseActiveSnapshot(ctx, namespace, memKey, getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountMem)); err != nil {
		errs = append(errs, err)
	}
	if err := s.releaseViewSnapshot(ctx, namespace, getVMViewSnapshotKey(key), getActiveMountPath(s.workDir, namespace, key, common.SnapshotMountVM)); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// Close releases snapshot resources.
func (s *Server) Close() error {
	return nil
}

// SnapshotInfo returns snapshot metadata, preferring active runtime state.
func (s *Server) SnapshotInfo(ctx context.Context, namespace, key string) (snapshots.Info, error) {
	if info := s.getActiveSnapshot(namespace, key); info != nil {
		return *info, nil
	}
	return s.snt.Stat(ctx, namespace, key)
}

// withLabels creates a snapshot option with labels from layout metadata.
func withLabels(labels map[string]string) snapshots.Opt {
	return func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for k, v := range labels {
			info.Labels[k] = v
		}
		return nil
	}
}
