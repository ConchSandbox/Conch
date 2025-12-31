package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"conch/core/snapshot/common"
	"conch/core/snapshot/rootfs"
	"conch/core/snapshot/snapshotter"

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

const (
	MemFileName = "mem.img"
)

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
		delete(m, key)
	}
}

func prepareSnapshotFiles(conf *SnapshotConfig) error {
	snRootDir := filepath.Join(conf.Rootfs, conf.RootDir)
	if err := os.MkdirAll(snRootDir, common.DirMode); err != nil {
		return err
	}
	// TODO: fusefs maybe should create new memfile
	memFile := conf.SnapshotMemFile()
	if _, err := os.Stat(memFile); err == nil {
		return nil
	}
	f, err := os.OpenFile(memFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, common.FileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(conf.MemSize * common.MEMMB); err != nil {
		return err
	}
	return nil
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

	// Step 1: prepare working layer
	mounts, err := s.snt.Prepare(ctx, namespace, key, parent, func(info *snapshots.Info) error {
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
		if err != nil {
			s.snt.Remove(ctx, namespace, key)
		}
	}()

	if len(mounts) != 1 {
		return nil, fmt.Errorf("overlayfs require only one mount info, but get: %v", mounts)
	}
	// Step 2: do overlay mount for layers
	if err := os.MkdirAll(conf.Rootfs, common.DirMode); err != nil {
		return nil, err
	}
	if err := mounts[0].Mount(conf.Rootfs); err != nil {
		fmt.Printf("mount: %s, err: %v", conf.Rootfs, err)
		return nil, err
	}
	defer func() {
		if err != nil {
			mount.Unmount(conf.Rootfs, unix.MNT_FORCE)
		}
	}()

	// Step 3: prepare vm snapshot dir
	if err = prepareSnapshotFiles(conf); err != nil {
		fmt.Printf("prepare vm snapshot dir: %s, err: %v", conf.RootDir, err)
		return nil, err
	}

	// Step 4: add vm snapshot dir into fusefs
	// TODO

	// Step 5: start virtio-fsd
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

	result, err := s.snt.Stat(ctx, namespace, key)
	if err != nil {
		return nil, err
	}
	s.addSnapshot(namespace, key, &result)

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

// only create local snapshot,
// if want image, we should generate digest for this snapshot, and update key
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

	// Step 1: commit snapshot into snapshotter
	// after commit, snapshotter will auto remove snapshotter of key
	err := s.snt.Commit(ctx, namespace, key, name, func(info *snapshots.Info) error {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for k, v := range conf.Labels {
			info.Labels[k] = v
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Step 2: stop rootfsd, disable vm write data into rootfs
	// but memfile should disable too
	in, ok := s.rootfs.GetInstance(fmt.Sprintf("%s-%s", namespace, key))
	if ok {
		fmt.Printf("TODO: disable rootfs/memory write data for: %s\n", in.Key)
	}

	// Step 3: update info
	result, err := s.snt.Stat(ctx, namespace, name)
	if err != nil {
		// warning: new snapshot add to snapshotter, but we lost it
		return err
	}
	s.addSnapshot(namespace, name, &result)

	return nil
}

func (s *server) Remove(ctx context.Context, namespace, key string) error {
	si := s.getSnapshot(namespace, key)
	if si == nil {
		return fmt.Errorf("snapshot [%s:%s] not found", namespace, key)
	}
	rootfs, ok := si.Labels[snapshotter.SnapshotLabelRootfs]
	// Step 1: clean mount of rootfs
	if ok {
		if err := mount.Unmount(rootfs, unix.MNT_FORCE); err != nil {
			return err
		}
	}
	// Step 2: remove snapshotter
	if _, err := s.snt.Stat(ctx, namespace, key); err == nil {
		err := s.snt.Remove(ctx, namespace, key)
		if err != nil {
			return err
		}
	}
	s.removeSnapshot(namespace, key)

	// Step 3: remove rootfs
	rootfsID := fmt.Sprintf("%s-%s", namespace, key)
	in, ok := s.rootfs.GetInstance(rootfsID)
	if ok {
		if err := in.Stop(); err != nil {
			return err
		}
		if err := s.rootfs.RemoveInstance(rootfsID); err != nil {
			return err
		}
	}

	return nil
}

func (c *SnapshotConfig) init(workDir, ns, key string) {
	if c.MemSize <= 0 {
		c.MemSize = common.MemFileDefaultSize
	}
	if c.RootDir == "" {
		c.RootDir = "/conch/snapshot"
	}
	if c.Rootfs == "" {
		c.Rootfs = filepath.Join(workDir, ns, key)
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
	return filepath.Join(c.Rootfs, c.RootDir, MemFileName)
}

func (c *SnapshotConfig) FullRootDir() string {
	return filepath.Join(c.Rootfs, c.RootDir)
}
