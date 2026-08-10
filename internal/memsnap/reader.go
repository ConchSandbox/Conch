package memsnap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

var openatForPin = unix.Openat

// PinnedManifest keeps validated layer descriptors open. Replacing a path
// after validation therefore cannot change the bytes returned to the guest.
type PinnedManifest struct {
	Manifest Manifest
	layers   map[int]*os.File
}

func LoadAndPin(root string) (*PinnedManifest, error) {
	return loadAndPin(root, nil)
}

// LoadAndPinEpoch pins only the newest layer. Parent layers may live in
// immutable parent OCI layers and need not be present in this epoch directory.
func LoadAndPinEpoch(root string) (*PinnedManifest, error) {
	return loadAndPin(root, func(manifest Manifest) []int {
		return []int{len(manifest.Layers) - 1}
	})
}

func loadAndPin(root string, selectLayers func(Manifest) []int) (*PinnedManifest, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest root: %w", err)
	}
	rootFD, err := openDirectoryNoSymlinks(resolvedRoot, false)
	if err != nil {
		return nil, fmt.Errorf("open manifest root: %w", err)
	}
	defer unix.Close(rootFD)

	manifestFile, _, err := openRegularAt(rootFD, ManifestFileName)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	data, readErr := io.ReadAll(manifestFile)
	closeErr := manifestFile.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read manifest: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close manifest: %w", closeErr)
	}
	var manifest Manifest
	if err := decodeStrictJSON(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}

	indexes := make([]int, len(manifest.Layers))
	for index := range manifest.Layers {
		indexes[index] = index
	}
	if selectLayers != nil {
		indexes = selectLayers(manifest)
	}
	pinned := &PinnedManifest{Manifest: manifest, layers: make(map[int]*os.File, len(indexes))}
	for _, index := range indexes {
		if index < 0 || index >= len(manifest.Layers) {
			_ = pinned.Close()
			return nil, fmt.Errorf("invalid selected layer index %d", index)
		}
		file, stat, err := openRegularAt(rootFD, manifest.Layers[index])
		if err != nil {
			_ = pinned.Close()
			return nil, fmt.Errorf("open layer %d: %w", index, err)
		}
		if stat.Size < 0 || uint64(stat.Size) != manifest.MemorySize {
			_ = file.Close()
			_ = pinned.Close()
			return nil, fmt.Errorf("layer %d has invalid logical size", index)
		}
		pinned.layers[index] = file
	}
	return pinned, nil
}

func openDirectoryNoSymlinks(path string, create bool) (int, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return -1, err
	}
	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	trimmed := strings.TrimPrefix(filepath.Clean(absolute), string(filepath.Separator))
	if trimmed == "" {
		return current, nil
	}
	for _, component := range strings.Split(trimmed, string(filepath.Separator)) {
		next, err := openDirectoryAt(current, component, create)
		_ = unix.Close(current)
		if err != nil {
			return -1, err
		}
		current = next
	}
	return current, nil
}

func openDirectoryAt(parentFD int, name string, create bool) (int, error) {
	var inspected unix.Stat_t
	err := unix.Fstatat(parentFD, name, &inspected, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) && create {
		if mkdirErr := unix.Mkdirat(parentFD, name, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
			return -1, mkdirErr
		}
		err = unix.Fstatat(parentFD, name, &inspected, unix.AT_SYMLINK_NOFOLLOW)
	}
	if err != nil {
		return -1, err
	}
	if inspected.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, fmt.Errorf("path component %q is not a directory", name)
	}
	openedFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(openedFD, &opened); err != nil {
		_ = unix.Close(openedFD)
		return -1, err
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFDIR || !sameFileIdentity(inspected, opened) {
		_ = unix.Close(openedFD)
		return -1, fmt.Errorf("path component %q changed during open", name)
	}
	return openedFD, nil
}

func openRegularAt(rootFD int, path string) (*os.File, unix.Stat_t, error) {
	var zero unix.Stat_t
	components := strings.Split(filepath.ToSlash(path), "/")
	if len(components) == 0 || path == "" || filepath.IsAbs(path) {
		return nil, zero, fmt.Errorf("invalid relative path %q", path)
	}
	current, err := unix.Dup(rootFD)
	if err != nil {
		return nil, zero, err
	}
	unix.CloseOnExec(current)
	for _, component := range components[:len(components)-1] {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(current)
			return nil, zero, fmt.Errorf("invalid path component %q", component)
		}
		next, err := openDirectoryAt(current, component, false)
		_ = unix.Close(current)
		if err != nil {
			return nil, zero, err
		}
		current = next
	}
	defer unix.Close(current)

	leaf := components[len(components)-1]
	if leaf == "" || leaf == "." || leaf == ".." {
		return nil, zero, fmt.Errorf("invalid path leaf %q", leaf)
	}
	var inspected unix.Stat_t
	if err := unix.Fstatat(current, leaf, &inspected, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, zero, err
	}
	if inspected.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, zero, fmt.Errorf("path leaf %q is not a regular file", leaf)
	}
	openedFD, err := openatForPin(current, leaf, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, zero, err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(openedFD, &opened); err != nil {
		_ = unix.Close(openedFD)
		return nil, zero, err
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || !sameFileIdentity(inspected, opened) {
		_ = unix.Close(openedFD)
		return nil, zero, fmt.Errorf("path leaf %q changed during open", leaf)
	}
	file := os.NewFile(uintptr(openedFD), leaf)
	if file == nil {
		_ = unix.Close(openedFD)
		return nil, zero, fmt.Errorf("wrap opened file %q", leaf)
	}
	return file, opened, nil
}

func sameFileIdentity(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino
}

func (p *PinnedManifest) ReadPage(offset uint64, dst []byte) error {
	if p == nil {
		return errors.New("nil pinned manifest")
	}
	if uint64(len(dst)) != p.Manifest.BlockSize || offset%p.Manifest.BlockSize != 0 || offset >= p.Manifest.MemorySize {
		return fmt.Errorf("invalid page offset or size")
	}
	index := sort.Search(len(p.Manifest.BuildMap), func(index int) bool {
		span := p.Manifest.BuildMap[index]
		return span.Offset+span.Length > offset
	})
	if index == len(p.Manifest.BuildMap) || offset < p.Manifest.BuildMap[index].Offset {
		return fmt.Errorf("no layer owns page at offset %d", offset)
	}
	layerIndex := p.Manifest.BuildMap[index].LayerIndex
	file := p.layers[layerIndex]
	if file == nil {
		return fmt.Errorf("layer %d is not pinned", layerIndex)
	}
	if _, err := io.ReadFull(io.NewSectionReader(file, int64(offset), int64(len(dst))), dst); err != nil {
		return fmt.Errorf("read page at offset %d: %w", offset, err)
	}
	return nil
}

func (p *PinnedManifest) Close() error {
	if p == nil {
		return nil
	}
	var result error
	for index, file := range p.layers {
		if err := file.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close layer %d: %w", index, err))
		}
		delete(p.layers, index)
	}
	return result
}
