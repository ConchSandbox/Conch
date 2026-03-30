package snapshot

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/containerd/containerd/snapshots"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

type Kind uint8

const (
	KindUnknown Kind = iota
	KindRW
	KindSnapshot
	KindImage
)

type SnapshotConfig struct {
	Rootfs string // dir which mount rootfs overlayfs
	MemDir string // dir which mount mem overlayfs
	VmDir  string // dir which mount vm snapshot overlayfs

	RootDir string // dir which save vm snapshot (inside MemDir)
	MemSize int64  // memory size of vm, unit is mb

	pmemFiles []string // pmem array (e.g. layer1.erofs, layer2.erofs, layer3.erofs)

	Labels map[string]string // other labels add into info
}

func (c *SnapshotConfig) PmemFiles() []string {
	result := make([]string, 0, len(c.pmemFiles))
	for _, name := range c.pmemFiles {
		result = append(result, filepath.Join(c.Rootfs, name))
	}
	return result
}

func (c *SnapshotConfig) SnapshotMemFile() string {
	return filepath.Join(c.MemDir, common.MemFileName)
}

func (c *SnapshotConfig) InitrdFile() string {
	return filepath.Join(c.VmDir, common.VmInitrdRelativePath)
}

func (c *SnapshotConfig) KernelFile() string {
	return filepath.Join(c.VmDir, common.VmKernelRelativePath)
}

func (c *SnapshotConfig) SnapDir() string {
	return filepath.Join(c.MemDir, c.RootDir)
}

// initDefaults sets default values for SnapshotConfig fields.
func (c *SnapshotConfig) initDefaults() {
	if c.MemSize <= 0 {
		c.MemSize = common.MemFileDefaultSize
	}
	if c.RootDir == "" {
		c.RootDir = "/conch/snapshot"
	}
	if c.pmemFiles == nil {
		c.pmemFiles = make([]string, 0)
	}
	if c.Labels == nil {
		c.Labels = make(map[string]string)
	}
}

// createLabels populates Labels with snapshot metadata.
func (c *SnapshotConfig) createLabels() {
	c.Labels[common.SnapshotLabel] = "true"
	c.Labels[common.SnapshotLabelMemSize] = fmt.Sprintf("%d", c.MemSize)
	c.Labels[common.SnapshotLabelRootfs] = c.Rootfs
	c.Labels[common.SnapshotLabelSnapshotDir] = c.RootDir
}

type Opt func(info *SnapshotConfig) error

// ParentSnapshotIDs groups parent snapshot IDs for image-based startup.
type ParentSnapshotIDs struct {
	Rootfs string
	Mem    string
	VM     string
}

// Prepare creates 3 new active snapshots for image-based startup.
func Prepare(ctx context.Context, namespace, key string, parents ParentSnapshotIDs, opts ...Opt) (*SnapshotConfig, error) {
	if gServer.snt == nil {
		return nil, fmt.Errorf("server not init")
	}
	return gServer.Prepare(ctx, namespace, key, parents, opts...)
}

// AcquireView views and mounts 3 committed snapshots for snapshot-based startup.
// Reuses existing view mounts if already mounted (refCount++).
func AcquireView(ctx context.Context, namespace, key string, parents ParentSnapshotIDs, opts ...Opt) (*SnapshotConfig, error) {
	if gServer.snt == nil {
		return nil, fmt.Errorf("server not init")
	}
	return gServer.AcquireView(ctx, namespace, key, parents, opts...)
}

// AcquireResumeWorkspace prepares a snapshot-based restore workspace.
// Rootfs and VM are mounted as shared views, while mem is mounted as an active
// layer so the snapshot config can be updated before restore.
func AcquireResumeWorkspace(ctx context.Context, namespace, key string, parents ParentSnapshotIDs, opts ...Opt) (*SnapshotConfig, error) {
	if gServer.snt == nil {
		return nil, fmt.Errorf("server not init")
	}
	return gServer.AcquireResumeWorkspace(ctx, namespace, key, parents, opts...)
}

// ResolveParentSnapshotIDs resolves parent mem/vm snapshots from rootfs snapshot.
func ResolveParentSnapshotIDs(namespace, rootfs string) (ParentSnapshotIDs, error) {
	if gServer.snt == nil {
		return ParentSnapshotIDs{}, fmt.Errorf("server not init")
	}
	return gServer.ResolveParentSnapshotIDs(namespace, rootfs)
}

// ResolveImageParentSnapshotIDs resolves image startup parents from a rootfs snapshot.
// Unlike snapshot resume, image startup allows mem label to be empty.
func ResolveImageParentSnapshotIDs(namespace, rootfs string) (ParentSnapshotIDs, error) {
	if gServer.snt == nil {
		return ParentSnapshotIDs{}, fmt.Errorf("server not init")
	}
	return gServer.ResolveImageParentSnapshotIDs(namespace, rootfs)
}

func Commit(ctx context.Context, namespace, snapshotID, key string, opts ...Opt) error {
	if gServer.snt == nil {
		return fmt.Errorf("server not init")
	}
	return gServer.Commit(ctx, namespace, snapshotID, key, opts...)
}

func Remove(ctx context.Context, namespace, key string) error {
	if gServer.snt == nil {
		return fmt.Errorf("server not init")
	}
	return gServer.Remove(ctx, namespace, key)
}

// CleanupAllViews unmounts and removes all view snapshots.
// Should be called during graceful shutdown before Close().
func CleanupAllViews() {
	gServer.CleanupAllViews()
}

// Close releases snapshot resources and closes provider if present.
func Close() error {
	if gServer.snt == nil {
		return nil
	}
	return gServer.Close()
}

// Stat gets snapshot information
func Stat(ctx context.Context, namespace, key string) (snapshots.Info, error) {
	if gServer.snt == nil {
		return snapshots.Info{}, fmt.Errorf("server not init")
	}
	return gServer.snt.Stat(ctx, namespace, key)
}
