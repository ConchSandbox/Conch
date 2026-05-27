package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/opencontainers/go-digest"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

const chainIDPrefix = "sha256:"

// CalculateSnapshotID calculates a snapshot ID from namespace, key, and parent.
// If parent is empty, returns a digest of namespace/key.
// If parent is set, returns a chain ID computed from parent and current layer.
func CalculateSnapshotID(namespace, key, parent string) (string, error) {
	if parent == "" {
		dgst := digest.FromString(fmt.Sprintf("%s/%s", namespace, key))
		return dgst.String(), nil
	}

	diffID := digest.FromString(fmt.Sprintf("%s/%s", namespace, key))
	chainID := calculateChainID(parent, diffID.String())
	return chainID, nil
}

// calculateChainID computes the chain ID for layer stacking.
func calculateChainID(parentChainID, diffID string) string {
	data := parentChainID + " " + diffID
	hash := sha256.Sum256([]byte(data))
	return chainIDPrefix + hex.EncodeToString(hash[:])
}

// prepareSnapshotFiles creates the snapshot directory structure.
func prepareSnapshotFiles(conf *SnapshotConfig) error {
	return os.MkdirAll(conf.SnapDir(), common.DirMode)
}

// listRootfsLayerErofs scans rootfs mount point for layer files in pattern "layer<N>.erofs".
// Returns sorted layer filenames by numeric index.
func listRootfsLayerErofs(rootfsMount string) ([]string, error) {
	entries, err := os.ReadDir(rootfsMount)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", rootfsMount, err)
	}

	const (
		layerPrefix = "layer"
		layerSuffix = ".erofs"
	)

	type layerEntry struct {
		name  string
		index int
	}
	layers := make([]layerEntry, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		// Check prefix and suffix
		if !strings.HasPrefix(name, layerPrefix) || !strings.HasSuffix(name, layerSuffix) {
			continue
		}

		// Extract numeric part: "layer123.erofs" -> "123"
		numPart := strings.TrimPrefix(name, layerPrefix)
		numPart = strings.TrimSuffix(numPart, layerSuffix)

		// Validate that the extracted part is a valid number
		idx, err := strconv.Atoi(numPart)
		if err != nil {
			// Skip files with non-numeric index (e.g., "layerX.erofs")
			continue
		}
		if idx < 0 {
			continue
		}

		layers = append(layers, layerEntry{name: name, index: idx})
	}

	if len(layers) == 0 {
		return []string{}, nil
	}

	// Sort by numeric index
	sort.Slice(layers, func(i, j int) bool {
		return layers[i].index < layers[j].index
	})

	result := make([]string, len(layers))
	for i, layer := range layers {
		result[i] = layer.name
	}
	return result, nil
}

// prepareSparseMemfile creates a sparse memory file of the specified size.
func prepareSparseMemfile(conf *SnapshotConfig, targetDir string) error {
	memFile := filepath.Join(targetDir, common.MemFileName)
	if err := os.MkdirAll(filepath.Dir(memFile), common.DirMode); err != nil {
		return err
	}

	f, err := os.OpenFile(memFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, common.FileMode)
	if err != nil {
		return fmt.Errorf("open memfile: %w", err)
	}
	defer f.Close()

	if err := f.Truncate(conf.MemSize * common.MemMB); err != nil {
		return fmt.Errorf("truncate memfile: %w", err)
	}

	return nil
}

// ensureMemFile checks for mem.img existence; creates a sparse file if createIfMissing is true.
func ensureMemFile(conf *SnapshotConfig, memDir string, createIfMissing bool) error {
	memFile := filepath.Join(memDir, common.MemFileName)
	if _, err := os.Stat(memFile); err == nil {
		return nil
	}
	if !createIfMissing {
		return fmt.Errorf("mem.img not found at %s", memFile)
	}
	return prepareSparseMemfile(conf, memDir)
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

func getSharedMountPath(workDir, namespace, snapshotID string) string {
	return filepath.Join(getSnapshotBasePath(workDir, namespace), common.SnapshotSharedDir, snapshotPathName(snapshotID))
}

// getMemKeyFromRootfs derives the mem snapshot key from rootfs key.
func getMemKeyFromRootfs(rootfsKey string) string {
	return rootfsKey + common.MemKeySuffix
}

func MemKeyFromRootfs(rootfsKey string) string {
	return getMemKeyFromRootfs(rootfsKey)
}

func getRootfsViewAliasKey(sandboxID string) string {
	return fmt.Sprintf("view-%s-%s", common.SnapshotMountRootfs, sandboxID)
}

func getMemViewAliasKey(sandboxID string) string {
	return fmt.Sprintf("view-%s-%s", common.SnapshotMountMem, sandboxID)
}

func getVMViewAliasKey(sandboxID string) string {
	return fmt.Sprintf("view-%s-%s", common.SnapshotMountVM, sandboxID)
}

func getSharedViewSnapshotKey(mountKind, snapshotID string) string {
	return fmt.Sprintf("shared-%s-%s", mountKind, snapshotPathName(snapshotID))
}

// cleanupEmptySnapshotParents removes empty parent directories after a mount point
// directory has been deleted. It only prunes within the snapshot tree and stops
// at the "snapshot" root directory.
func cleanupEmptySnapshotParents(mountPoint string) error {
	dir := filepath.Dir(mountPoint)
	for {
		base := filepath.Base(dir)
		if base == "." || base == string(filepath.Separator) || base == "snapshot" {
			return nil
		}
		if filepath.Base(filepath.Dir(dir)) == "snapshot" {
			return nil
		}

		err := os.Remove(dir)
		if err == nil {
			dir = filepath.Dir(dir)
			continue
		}
		if os.IsNotExist(err) {
			dir = filepath.Dir(dir)
			continue
		}
		// Non-empty directories stop the prune quietly; other failures bubble up.
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		}
		return err
	}
}

// mergeLabels merges snapshot info labels into config.
func mergeLabels(info *snapshots.Info, conf *SnapshotConfig) {
	for k, v := range info.Labels {
		switch k {
		case common.SnapshotLabelMemSize:
			mSize, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				conf.MemSize = mSize
			}
		case common.SnapshotLabelRootfs:
			conf.Rootfs = v
		case common.SnapshotLabelSnapshotDir:
			conf.RootDir = v
		default:
			conf.Labels[k] = v
		}
	}
}
