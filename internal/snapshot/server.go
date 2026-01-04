package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/snapshot/rootfs"
	"github.com/openeuler/Conch/internal/snapshot/snapshotter"
	"github.com/openeuler/Conch/internal/utils"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"golang.org/x/sys/unix"
)

type server struct {
	snt    snapshotter.Snapshotter
	rootfs *rootfs.Manager

	snapshots map[string]map[string]*snapshots.Info
	lock      sync.RWMutex
	workDir   string
}

var gServer server

var noGcOpt = snapshots.WithLabels(map[string]string{
	"containerd.io/gc.root": time.Now().UTC().Format(time.RFC3339),
})

func NewServer(workDir string) error {
	sn, err := snapshotter.NewContainerdSnap()
	if err != nil {
		return err
	}
	gServer.rootfs = rootfs.NewManager()
	gServer.snt = sn
	gServer.workDir = workDir
	gServer.snapshots = make(map[string]map[string]*snapshots.Info)

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

func (s *server) getSnapshot(ns, key string) *snapshots.Info {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if m, ok := s.snapshots[ns]; ok {
		return m[key]
	}
	return nil
}

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

func (s *server) removeSnapshot(ns, key string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if m, ok := s.snapshots[ns]; ok {
		memKey := getMemKeyFromRootfs(key)
		delete(m, memKey)
		delete(m, key)
	}
}

func prepareSnapshotFiles(conf *SnapshotConfig) error {
	if err := os.MkdirAll(conf.FullRootDir(), common.DirMode); err != nil {
		return err
	}
	return nil
}

func (s *server) prepareAndMountSnapshot(
	ctx context.Context,
	locator SnapshotLocator,
	mountPoint string,
	opts ...snapshots.Opt,
) (*snapshotCleaner, error) {
	var err error

	cleaner := &snapshotCleaner{
		ctx:        ctx,
		server:     s,
		namespace:  locator.Namespace,
		key:        locator.Key,
		mountPoint: mountPoint,
	}

	defer func() {
		if err != nil {
			cleaner.Cleanup()
		}
	}()

	mounts, err := s.snt.Prepare(ctx, locator.Namespace, locator.Key, locator.Parent, opts...)
	if err != nil {
		return nil, err
	}
	cleaner.prepared = true

	if len(mounts) != 1 {
		return nil, fmt.Errorf("overlayfs require only one mount info, but get: %v", mounts)
	}

	if err = os.MkdirAll(mountPoint, common.DirMode); err != nil {
		return nil, err
	}
	cleaner.dirCreated = true

	if err = mounts[0].Mount(mountPoint); err != nil {
		return nil, fmt.Errorf("mount snapshot %v failed: %v", locator.Key, err)
	}
	cleaner.mounted = true

	return cleaner, nil
}

func (s *server) viewAndMountSnapshot(
	ctx context.Context,
	locator SnapshotLocator,
	mountPoint string,
) (*snapshotCleaner, error) {
	var err error

	cleaner := &snapshotCleaner{
		ctx:        ctx,
		server:     s,
		namespace:  locator.Namespace,
		key:        locator.Key,
		mountPoint: mountPoint,
	}

	defer func() {
		if err != nil {
			cleaner.Cleanup()
		}
	}()

	mounts, err := s.snt.View(ctx, locator.Namespace, locator.Key, locator.Parent)
	if err != nil {
		return nil, fmt.Errorf("view snapshot %v failed: %v", locator.Key, err)
	}
	cleaner.prepared = true

	if err = os.MkdirAll(mountPoint, common.DirMode); err != nil {
		return nil, err
	}
	cleaner.dirCreated = true

	if err = mounts[0].Mount(mountPoint); err != nil {
		return nil, fmt.Errorf("mount snapshot %v failed: %v", locator.Key, err)
	}
	cleaner.mounted = true

	return cleaner, nil
}

func prepareMemfile(conf *SnapshotConfig, parentMemDir string) error {
	memFile := conf.SnapshotMemFile()
	if parentMemDir != "" {
		parentMemFile := filepath.Join(parentMemDir, common.MemFileName)
		if _, err := os.Stat(parentMemFile); err == nil {
			// copy file
			return copyFile(parentMemFile, memFile)
		}
	}

	// Create sparse file
	f, err := os.OpenFile(memFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, common.FileMode)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Truncate(conf.MemSize * common.MemMB); err != nil {
		return err
	}

	return nil
}

func copyFile(src, dst string) error {
	// Use reflink if filesystem supports (XFS/Btrfs), fallback to sparse copy
	cmd := exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst)
	return cmd.Run()
}

func (s *server) prepareParentMemView(ctx context.Context, namespace, memKey, parent string) (parentMemDir string, _ *snapshotCleaner, err error) {
	if parent == "" {
		return "", nil, nil
	}

	memParent, err := s.getMemNameFromRootfs(namespace, parent)
	if err != nil {
		return "", nil, nil
	}

	viewKey := common.TempViewPrefix + memKey
	parentMemViewLocator := NewSnapshotLocator(namespace, viewKey, memParent)
	parentMemDir = getMountPath(s.workDir, parentMemViewLocator.Namespace, parentMemViewLocator.Key)
	viewCleanup, err := s.viewAndMountSnapshot(ctx, parentMemViewLocator, parentMemDir)
	if err != nil {
		return "", nil, fmt.Errorf("view parent mem snapshot failed: %v", err)
	}

	return parentMemDir, viewCleanup, nil
}

func (s *server) Prepare(ctx context.Context, namespace, key, parent string, opts ...Opt) (_ *SnapshotConfig, err error) {
	conf := &SnapshotConfig{}
	if si := s.getSnapshot(namespace, key); si != nil {
		return nil, fmt.Errorf("snapshot [%s:%s] existed", namespace, key)
	}
	conf.init(s.workDir, namespace, key)
	for _, o := range opts {
		o(conf)
	}

	conf.createLabels()

	rootfsLocator := NewSnapshotLocator(namespace, key, parent)
	rootfsCleanup, err := s.prepareAndMountSnapshot(ctx, rootfsLocator, conf.Rootfs, noGcOpt, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for k, v := range conf.Labels {
			info.Labels[k] = v
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && rootfsCleanup != nil {
			rootfsCleanup.Cleanup()
		}
	}()

	if err = prepareSnapshotFiles(conf); err != nil {
		return nil, fmt.Errorf("prepare vm snapshot files failed: %v", err)
	}

	memKey := getMemKeyFromRootfs(key)
	memLocator := NewSnapshotLocator(namespace, memKey, "")
	memCleanup, err := s.prepareAndMountSnapshot(ctx, memLocator, conf.MemDir, noGcOpt)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && memCleanup != nil {
			memCleanup.Cleanup()
		}
	}()
	parentMemDir, viewCleanup, err := s.prepareParentMemView(ctx, namespace, memKey, parent)
	if err != nil {
		return nil, err
	}
	defer func() {
		if viewCleanup != nil {
			viewCleanup.Cleanup()
		}
	}()

	if err := prepareMemfile(conf, parentMemDir); err != nil {
		return nil, fmt.Errorf("prepare mem.img failed: %v", err)
	}

	// Step 7: start virtio-fsd
	rconf := &rootfs.Config{
		Key:    fmt.Sprintf("%s-%s", namespace, key),
		Rootfs: conf.Rootfs,
	}
	in, err := s.rootfs.NewInstance(rconf)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			s.rootfs.RemoveInstance(in.Key)
		}
	}()
	if err := in.Start(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			in.Stop()
		}
	}()
	conf.RootfsSock = in.GetRootfsSock()

	// Step 8: Update snapshot info
	result, err := s.snt.Stat(ctx, namespace, key)
	if err != nil {
		return nil, err
	}
	s.addSnapshot(namespace, key, &result)

	memResult, err := s.snt.Stat(ctx, namespace, memKey)
	if err != nil {
		return nil, err
	}
	s.addSnapshot(namespace, memKey, &memResult)

	return conf, nil
}

func mergeLables(info *snapshots.Info, conf *SnapshotConfig) {
	for k, v := range info.Labels {
		switch k {
		case snapshotter.SnapshotLabelMemSize:
			mSize, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				conf.MemSize = mSize
			}
		case snapshotter.SnapshotLabelRootfs:
			conf.Rootfs = v
		case snapshotter.SnapshotLabelSnapshotDir:
			conf.RootDir = v
		default:
			conf.Labels[k] = v
		}
	}
}

// Commit snapshot with externally calculated name
func (s *server) Commit(ctx context.Context, namespace, name, key string, opts ...Opt) error {
	si := s.getSnapshot(namespace, key)
	if si == nil {
		return fmt.Errorf("snapshot [%s:%s] not found", namespace, key)
	}

	conf := &SnapshotConfig{}
	conf.init(s.workDir, namespace, key)
	mergeLables(si, conf)
	for _, o := range opts {
		o(conf)
	}
	conf.createLabels()

	// Get mem snapshot key
	memKey := getMemKeyFromRootfs(key)
	memInfo := s.getSnapshot(namespace, memKey)
	if memInfo == nil {
		return fmt.Errorf("mem snapshot [%s:%s] not found", namespace, memKey)
	}
	// Calculate mem snapshot name
	memName, err := utils.CalculateSnapshotName(namespace, memKey, "")
	if err != nil {
		return fmt.Errorf("calculate mem snapshot name failed: %v", err)
	}

	// Step 1: commit rootfs snapshot
	// Name is required (compute externally)
	if name == "" {
		return fmt.Errorf("rootfs snapshot name is required (compute externally)")
	}
	rootfsName := name

	err = s.snt.Commit(ctx, namespace, key, rootfsName, noGcOpt, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for k, v := range conf.Labels {
			info.Labels[k] = v
		}
		info.Labels[snapshotter.SnapshotLabelMemSnapshot] = memName
		return nil
	})
	if err != nil {
		return err
	}

	// Step 2: commit mem snapshot
	err = s.snt.Commit(ctx, namespace, memKey, memName, noGcOpt, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[snapshotter.SnapshotLabelRootfsSnapshot] = rootfsName
		return nil
	})
	if err != nil {
		return fmt.Errorf("commit mem snapshot failed: %v", err)
	}

	// Step 3: stop rootfsd
	rootfsID := fmt.Sprintf("%s-%s", namespace, key)
	in, ok := s.rootfs.GetInstance(rootfsID)
	if ok {
		fmt.Printf("stopping rootfs daemon for: %s\n", in.Key)
	}

	// Step 4: update info
	result, err := s.snt.Stat(ctx, namespace, rootfsName)
	if err != nil {
		return err
	}
	s.addSnapshot(namespace, rootfsName, &result)

	memResult, err := s.snt.Stat(ctx, namespace, memName)
	if err != nil {
		return err
	}
	s.addSnapshot(namespace, memName, &memResult)

	return nil
}

func (s *server) Remove(ctx context.Context, namespace, key string) error {
	si := s.getSnapshot(namespace, key)
	if si == nil {
		return fmt.Errorf("snapshot [%s:%s] not found", namespace, key)
	}

	var errs []error

	memKey := getMemKeyFromRootfs(key)

	// Step 1: clean mount of rootfs
	if rootfs, ok := si.Labels[snapshotter.SnapshotLabelRootfs]; ok {
		if err := mount.Unmount(rootfs, unix.MNT_FORCE); err != nil {
			errs = append(errs, fmt.Errorf("unmount rootfs %s: %w", rootfs, err))
		}
	}

	// Step 2: clean mount of mem snapshot
	memDir := getMountPath(s.workDir, namespace, memKey)
	if err := mount.Unmount(memDir, unix.MNT_FORCE); err != nil {
		errs = append(errs, fmt.Errorf("unmount mem %s: %w", memDir, err))
	}

	// Step 3: remove rootfs snapshot
	if _, err := s.snt.Stat(ctx, namespace, key); err == nil {
		if err := s.snt.Remove(ctx, namespace, key); err != nil {
			errs = append(errs, fmt.Errorf("remove rootfs snapshot %s: %w", key, err))
		}
	}
	s.removeSnapshot(namespace, key)

	// Step 4: remove mem snapshot
	if _, err := s.snt.Stat(ctx, namespace, memKey); err == nil {
		if err := s.snt.Remove(ctx, namespace, memKey); err != nil {
			errs = append(errs, fmt.Errorf("remove mem snapshot %s: %w", memKey, err))
		}
	}
	s.removeSnapshot(namespace, memKey)

	// Step 5: remove rootfs instance
	rootfsID := fmt.Sprintf("%s-%s", namespace, key)
	if in, ok := s.rootfs.GetInstance(rootfsID); ok {
		if err := in.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop rootfs instance %s: %w", rootfsID, err))
		}
		if err := s.rootfs.RemoveInstance(rootfsID); err != nil {
			errs = append(errs, fmt.Errorf("remove rootfs instance %s: %w", rootfsID, err))
		}
	}

	return errors.Join(errs...)
}

func (c *SnapshotConfig) init(workDir, ns, key string) {
	if c.MemSize <= 0 {
		c.MemSize = common.MemFileDefaultSize
	}
	if c.RootDir == "" {
		c.RootDir = "/conch/snapshot"
	}
	if c.Rootfs == "" {
		c.Rootfs = getMountPath(workDir, ns, key)
	}
	if c.MemDir == "" {
		memKey := getMemKeyFromRootfs(key)
		c.MemDir = getMountPath(workDir, ns, memKey)
	}
	if c.Labels == nil {
		c.Labels = make(map[string]string)
	}
}

func (c *SnapshotConfig) createLabels() {
	c.Labels[snapshotter.SnapshotLabel] = "true"
	c.Labels[snapshotter.SnapshotLabelMemSize] = strconv.FormatInt(c.MemSize, 10)
	c.Labels[snapshotter.SnapshotLabelRootfs] = c.Rootfs
	c.Labels[snapshotter.SnapshotLabelSnapshotDir] = c.RootDir
}

func (c *SnapshotConfig) SnapshotMemFile() string {
	return filepath.Join(c.MemDir, common.MemFileName)
}

func (c *SnapshotConfig) FullRootDir() string {
	return filepath.Join(c.Rootfs, c.RootDir)
}

func getMountPath(workDir, namespace, key string) string {
	return filepath.Join(workDir, namespace, key)
}

func getMemKeyFromRootfs(rootfsKey string) string {
	return rootfsKey + common.MemKeySuffix
}

func (s *server) getMemNameFromRootfs(namespace, rootfsName string) (string, error) {
	info := s.getSnapshot(namespace, rootfsName)
	if info == nil {
		return "", fmt.Errorf("rootfs snapshot %s not found", rootfsName)
	}

	memName, ok := info.Labels[snapshotter.SnapshotLabelMemSnapshot]
	if !ok {
		return "", fmt.Errorf("mem snapshot name not found in rootfs %s labels", rootfsName)
	}

	return memName, nil
}
