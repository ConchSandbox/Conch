package memsnap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

type PageSink interface {
	WritePage(offset uint64, page []byte) error
	WriteZeroPage(offset uint64) error
}

func CreateBaseLayer(root string, memorySize, blockSize uint64, export func(PageSink) error) (Manifest, error) {
	if memorySize == 0 || blockSize == 0 || memorySize%blockSize != 0 {
		return Manifest{}, fmt.Errorf("invalid memory geometry")
	}
	layer, writer, err := createRawLayer(root, 0, memorySize, blockSize)
	if err != nil {
		return Manifest{}, err
	}
	if err := runExport(writer, export); err != nil {
		return Manifest{}, err
	}
	if uint64(len(writer.touched)) != memorySize/blockSize {
		_ = removeLayer(root, layer)
		return Manifest{}, fmt.Errorf("base export did not cover every page")
	}
	return Manifest{
		SchemaVersion: SchemaVersion,
		MemorySize:    memorySize,
		BlockSize:     blockSize,
		Layers:        []string{layer},
		BuildMap:      []BuildRange{{Offset: 0, Length: memorySize, LayerIndex: 0}},
	}, nil
}

func CreateDeltaLayer(parent Manifest, root string, export func(PageSink) error) (Manifest, error) {
	if err := validateManifest(parent); err != nil {
		return Manifest{}, fmt.Errorf("invalid parent manifest: %w", err)
	}
	layerIndex := len(parent.Layers)
	layer, writer, err := createRawLayer(root, layerIndex, parent.MemorySize, parent.BlockSize)
	if err != nil {
		return Manifest{}, err
	}
	if err := runExport(writer, export); err != nil {
		return Manifest{}, err
	}
	return Manifest{
		SchemaVersion: SchemaVersion,
		MemorySize:    parent.MemorySize,
		BlockSize:     parent.BlockSize,
		Layers:        append(append([]string(nil), parent.Layers...), layer),
		BuildMap:      replaceOwnership(parent.BuildMap, writer.touchedOffsets(), parent.BlockSize, layerIndex),
	}, nil
}

type layerWriter struct {
	file        *os.File
	directoryFD int
	name        string
	memorySize  uint64
	blockSize   uint64
	touched     map[uint64]struct{}
}

func createRawLayer(root string, layerIndex int, memorySize, blockSize uint64) (string, *layerWriter, error) {
	rootFD, err := openDirectoryNoSymlinks(root, true)
	if err != nil {
		return "", nil, fmt.Errorf("open layer root: %w", err)
	}
	defer unix.Close(rootFD)
	directoryFD, err := openDirectoryAt(rootFD, LayerDirName, true)
	if err != nil {
		return "", nil, fmt.Errorf("open layer directory: %w", err)
	}
	name := fmt.Sprintf("%d.mem", layerIndex)
	fileFD, err := unix.Openat(directoryFD, name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		_ = unix.Close(directoryFD)
		return "", nil, fmt.Errorf("create layer: %w", err)
	}
	file := os.NewFile(uintptr(fileFD), name)
	if file == nil {
		_ = unix.Close(fileFD)
		_ = unix.Close(directoryFD)
		return "", nil, fmt.Errorf("wrap created layer")
	}
	writer := &layerWriter{
		file:        file,
		directoryFD: directoryFD,
		name:        name,
		memorySize:  memorySize,
		blockSize:   blockSize,
		touched:     make(map[uint64]struct{}),
	}
	if err := file.Truncate(int64(memorySize)); err != nil {
		writer.discard()
		return "", nil, fmt.Errorf("size layer: %w", err)
	}
	return filepath.ToSlash(filepath.Join(LayerDirName, name)), writer, nil
}

func runExport(writer *layerWriter, export func(PageSink) error) error {
	if export == nil {
		writer.discard()
		return fmt.Errorf("nil layer export")
	}
	if err := export(writer); err != nil {
		writer.discard()
		return fmt.Errorf("export layer: %w", err)
	}
	if err := writer.verifyPathIdentity(); err != nil {
		writer.discard()
		return fmt.Errorf("verify layer path: %w", err)
	}
	if err := writer.file.Sync(); err != nil {
		writer.discard()
		return fmt.Errorf("sync layer: %w", err)
	}
	if err := writer.file.Close(); err != nil {
		_ = unix.Unlinkat(writer.directoryFD, writer.name, 0)
		_ = unix.Close(writer.directoryFD)
		return fmt.Errorf("close layer: %w", err)
	}
	_ = unix.Close(writer.directoryFD)
	writer.directoryFD = -1
	return nil
}

func (writer *layerWriter) WritePage(offset uint64, page []byte) error {
	if uint64(len(page)) != writer.blockSize {
		return fmt.Errorf("page length %d does not match block size %d", len(page), writer.blockSize)
	}
	if err := writer.claim(offset); err != nil {
		return err
	}
	if isAllZero(page) {
		return nil
	}
	if _, err := writer.file.WriteAt(page, int64(offset)); err != nil {
		return fmt.Errorf("write page at offset %d: %w", offset, err)
	}
	return nil
}

func (writer *layerWriter) WriteZeroPage(offset uint64) error {
	return writer.claim(offset)
}

func (writer *layerWriter) claim(offset uint64) error {
	if writer.blockSize == 0 || offset%writer.blockSize != 0 || offset >= writer.memorySize {
		return fmt.Errorf("page offset %d is outside aligned guest memory", offset)
	}
	if _, found := writer.touched[offset]; found {
		return fmt.Errorf("page offset %d was written twice", offset)
	}
	writer.touched[offset] = struct{}{}
	return nil
}

func (writer *layerWriter) touchedOffsets() []uint64 {
	offsets := make([]uint64, 0, len(writer.touched))
	for offset := range writer.touched {
		offsets = append(offsets, offset)
	}
	sort.Slice(offsets, func(left, right int) bool { return offsets[left] < offsets[right] })
	return offsets
}

func (writer *layerWriter) verifyPathIdentity() error {
	var inspected unix.Stat_t
	if err := unix.Fstatat(writer.directoryFD, writer.name, &inspected, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(writer.file.Fd()), &opened); err != nil {
		return err
	}
	if inspected.Mode&unix.S_IFMT != unix.S_IFREG || opened.Mode&unix.S_IFMT != unix.S_IFREG || !sameFileIdentity(inspected, opened) {
		return fmt.Errorf("created layer changed identity")
	}
	return nil
}

func (writer *layerWriter) discard() {
	_ = writer.file.Close()
	if writer.directoryFD >= 0 {
		_ = unix.Unlinkat(writer.directoryFD, writer.name, 0)
		_ = unix.Close(writer.directoryFD)
		writer.directoryFD = -1
	}
}

func removeLayer(root, layer string) error {
	return os.Remove(filepath.Join(root, filepath.FromSlash(layer)))
}

func isAllZero(page []byte) bool {
	for _, value := range page {
		if value != 0 {
			return false
		}
	}
	return true
}

func replaceOwnership(parent []BuildRange, dirty []uint64, blockSize uint64, layerIndex int) []BuildRange {
	buildMap := make([]BuildRange, 0, len(parent)+len(dirty)*2)
	dirtyIndex := 0
	for _, parentRange := range parent {
		end := parentRange.Offset + parentRange.Length
		cursor := parentRange.Offset
		for dirtyIndex < len(dirty) && dirty[dirtyIndex] < end {
			dirtyOffset := dirty[dirtyIndex]
			if cursor < dirtyOffset {
				appendMergedRange(&buildMap, BuildRange{Offset: cursor, Length: dirtyOffset - cursor, LayerIndex: parentRange.LayerIndex})
			}
			appendMergedRange(&buildMap, BuildRange{Offset: dirtyOffset, Length: blockSize, LayerIndex: layerIndex})
			cursor = dirtyOffset + blockSize
			dirtyIndex++
		}
		if cursor < end {
			appendMergedRange(&buildMap, BuildRange{Offset: cursor, Length: end - cursor, LayerIndex: parentRange.LayerIndex})
		}
	}
	return buildMap
}

func appendMergedRange(buildMap *[]BuildRange, next BuildRange) {
	if len(*buildMap) != 0 {
		last := &(*buildMap)[len(*buildMap)-1]
		if last.LayerIndex == next.LayerIndex && last.Offset+last.Length == next.Offset {
			last.Length += next.Length
			return
		}
	}
	*buildMap = append(*buildMap, next)
}
