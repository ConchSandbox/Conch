package snapshot

import (
	"fmt"
	"path/filepath"
	"strings"

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
	Rootfs string // dir which mounts rootfs snapshot view
	MemDir string // dir which mounts mem snapshot view or active layer
	VmDir  string // dir which mounts sandbox snapshot view

	RootDir string // dir which save vm snapshot (inside MemDir)
	MemSize int64  // memory size of vm, unit is mb

	pmemFiles []string // pmem array (e.g. layer1.erofs, layer2.erofs, layer3.erofs)

	Labels map[string]string // other labels add into info
}

func (c *SnapshotConfig) PmemFiles() []string {
	result := make([]string, 0, len(c.pmemFiles))
	for _, name := range c.pmemFiles {
		if filepath.IsAbs(name) {
			result = append(result, name)
			continue
		}
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
	return filepath.Join(c.MemDir, strings.TrimLeft(c.RootDir, string(filepath.Separator)))
}

// initDefaults sets default values for SnapshotConfig fields.
func (c *SnapshotConfig) initDefaults() {
	if c.MemSize <= 0 {
		c.MemSize = common.MemFileDefaultSize
	}
	if c.RootDir == "" {
		c.RootDir = "conch/snapshot"
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
