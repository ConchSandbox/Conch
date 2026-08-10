package vmm

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/openeuler/Conch/internal/memsnap"
)

const memoryExportBatchSize = uint64(2 * 1024 * 1024)

type processMemory interface {
	io.ReaderAt
	io.Closer
}

type processMemoryOpener func(pid int) (processMemory, error)

type MemoryExporter struct {
	openMemory processMemoryOpener
	batchSize  uint64
}

func NewMemoryExporter() *MemoryExporter {
	return newMemoryExporter(func(pid int) (processMemory, error) {
		return os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	}, memoryExportBatchSize)
}

func newMemoryExporter(opener processMemoryOpener, batchSize uint64) *MemoryExporter {
	return &MemoryExporter{openMemory: opener, batchSize: batchSize}
}

type pageExportAction uint8

const (
	pageSkip pageExportAction = iota
	pageRead
	pageZero
)

type pageSelector func(offset uint64) pageExportAction

func (exporter *MemoryExporter) ExportPageState(
	ctx context.Context,
	pid int,
	memorySize, blockSize uint64,
	mappings []MemoryMapping,
	state MemoryPageState,
	sink memsnap.PageSink,
) error {
	if err := validateBitmap("resident", state.Resident, memorySize, state.PageSize); err != nil {
		return err
	}
	if err := validateBitmap("empty", state.Empty, memorySize, state.PageSize); err != nil {
		return err
	}
	for word, empty := range state.Empty {
		var resident uint64
		if word < len(state.Resident) {
			resident = state.Resident[word]
		}
		if empty&^resident != 0 {
			return fmt.Errorf("empty page bitmap contains pages not marked resident")
		}
	}
	selector := func(offset uint64) pageExportAction {
		page := offset / state.PageSize
		if bitmapBit(state.Resident, page) && !bitmapBit(state.Empty, page) {
			return pageRead
		}
		return pageZero
	}
	return exporter.export(ctx, pid, memorySize, blockSize, state.PageSize, mappings, selector, sink)
}

func (exporter *MemoryExporter) ExportDirtyBitmap(
	ctx context.Context,
	pid int,
	memorySize, blockSize uint64,
	mappings []MemoryMapping,
	dirty MemoryDirtyBitmap,
	sink memsnap.PageSink,
) error {
	if err := validateBitmap("dirty", dirty.Bitmap, memorySize, dirty.PageSize); err != nil {
		return err
	}
	selector := func(offset uint64) pageExportAction {
		if bitmapBit(dirty.Bitmap, offset/dirty.PageSize) {
			return pageRead
		}
		return pageSkip
	}
	return exporter.export(ctx, pid, memorySize, blockSize, dirty.PageSize, mappings, selector, sink)
}

func ExportMemoryByPageState(
	ctx context.Context,
	pid int,
	memorySize, blockSize uint64,
	mappings []MemoryMapping,
	state MemoryPageState,
	sink memsnap.PageSink,
) error {
	return NewMemoryExporter().ExportPageState(ctx, pid, memorySize, blockSize, mappings, state, sink)
}

func ExportMemoryByDirtyBitmap(
	ctx context.Context,
	pid int,
	memorySize, blockSize uint64,
	mappings []MemoryMapping,
	dirty MemoryDirtyBitmap,
	sink memsnap.PageSink,
) error {
	return NewMemoryExporter().ExportDirtyBitmap(ctx, pid, memorySize, blockSize, mappings, dirty, sink)
}

func (exporter *MemoryExporter) export(
	ctx context.Context,
	pid int,
	memorySize, blockSize, responsePageSize uint64,
	mappings []MemoryMapping,
	selector pageSelector,
	sink memsnap.PageSink,
) error {
	if err := validateExportGeometry(pid, memorySize, blockSize, responsePageSize, sink); err != nil {
		return err
	}
	if err := validateMemoryMappings(mappings, memorySize, blockSize, responsePageSize); err != nil {
		return err
	}
	if err := validateSelectedMappings(ctx, mappings, memorySize, blockSize, selector); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if exporter == nil || exporter.openMemory == nil {
		return fmt.Errorf("process memory opener is nil")
	}
	memory, err := exporter.openMemory(pid)
	if err != nil {
		return fmt.Errorf("open VMM process memory: %w", err)
	}
	if memory == nil {
		return fmt.Errorf("process memory opener returned nil")
	}
	defer memory.Close()

	batchSize := exporter.batchSize
	if batchSize == 0 || batchSize > memoryExportBatchSize {
		batchSize = memoryExportBatchSize
	}
	if batchSize < blockSize {
		batchSize = blockSize
	}
	batchSize -= batchSize % blockSize
	buffer := make([]byte, int(batchSize))

	for offset := uint64(0); offset < memorySize; {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch action := selector(offset); action {
		case pageSkip:
			offset += blockSize
		case pageZero:
			if err := sink.WriteZeroPage(offset); err != nil {
				return fmt.Errorf("write zero page at guest offset %d: %w", offset, err)
			}
			offset += blockSize
		case pageRead:
			mappingIndex, err := memoryMappingIndex(mappings, offset, blockSize)
			if err != nil {
				return err
			}
			mapping := mappings[mappingIndex]
			mappingEnd := mapping.Offset + mapping.Size
			length := blockSize
			for length < batchSize && offset+length < mappingEnd && selector(offset+length) == pageRead {
				length += blockSize
			}
			hostOffset := mapping.BaseHostVirtualAddress + offset - mapping.Offset
			data := buffer[:int(length)]
			n, readErr := memory.ReadAt(data, int64(hostOffset))
			if readErr != nil || n != len(data) {
				return fmt.Errorf("short VMM memory read at guest offset %d HVA 0x%x: read %d of %d: %w", offset, hostOffset, n, len(data), readErr)
			}
			for pageOffset := uint64(0); pageOffset < length; pageOffset += blockSize {
				if err := ctx.Err(); err != nil {
					return err
				}
				page := data[int(pageOffset):int(pageOffset+blockSize)]
				guestOffset := offset + pageOffset
				if isZeroMemoryPage(page) {
					if err := sink.WriteZeroPage(guestOffset); err != nil {
						return fmt.Errorf("write zero page at guest offset %d: %w", guestOffset, err)
					}
				} else if err := sink.WritePage(guestOffset, page); err != nil {
					return fmt.Errorf("write page at guest offset %d: %w", guestOffset, err)
				}
			}
			offset += length
		default:
			return fmt.Errorf("invalid page export action %d", action)
		}
	}
	return nil
}

func validateExportGeometry(pid int, memorySize, blockSize, responsePageSize uint64, sink memsnap.PageSink) error {
	if pid <= 0 {
		return fmt.Errorf("invalid VMM pid %d", pid)
	}
	if sink == nil {
		return fmt.Errorf("page sink is nil")
	}
	if memorySize == 0 {
		return fmt.Errorf("memory size is zero")
	}
	if !isPowerOfTwo(blockSize) || blockSize > memoryExportBatchSize {
		return fmt.Errorf("block size %d is not a supported power of two", blockSize)
	}
	if memorySize%blockSize != 0 {
		return fmt.Errorf("memory size %d is not aligned to block size %d", memorySize, blockSize)
	}
	if !isPowerOfTwo(responsePageSize) {
		return fmt.Errorf("response page size %d is not a power of two", responsePageSize)
	}
	if responsePageSize < blockSize || responsePageSize%blockSize != 0 || memorySize%responsePageSize != 0 {
		return fmt.Errorf("response page size %d is incompatible with memory size %d and block size %d", responsePageSize, memorySize, blockSize)
	}
	return nil
}

func validateMemoryMappings(mappings []MemoryMapping, memorySize, blockSize, responsePageSize uint64) error {
	if len(mappings) == 0 {
		return fmt.Errorf("memory mappings are empty")
	}
	var previousOffset, previousEnd uint64
	for index, mapping := range mappings {
		if mapping.Size == 0 {
			return fmt.Errorf("memory mapping %d has zero size", index)
		}
		if !isPowerOfTwo(mapping.PageSize) {
			return fmt.Errorf("memory mapping %d page size %d is not a power of two", index, mapping.PageSize)
		}
		if mapping.PageSize < blockSize || mapping.PageSize%blockSize != 0 {
			return fmt.Errorf("memory mapping %d page size %d is incompatible with block size %d", index, mapping.PageSize, blockSize)
		}
		if mapping.Offset%blockSize != 0 || mapping.Size%blockSize != 0 || mapping.BaseHostVirtualAddress%blockSize != 0 {
			return fmt.Errorf("memory mapping %d is not aligned to block size %d", index, blockSize)
		}
		if mapping.Offset%mapping.PageSize != 0 || mapping.Size%mapping.PageSize != 0 || mapping.BaseHostVirtualAddress%mapping.PageSize != 0 {
			return fmt.Errorf("memory mapping %d is not aligned to mapping page size %d", index, mapping.PageSize)
		}
		if mapping.Offset%responsePageSize != 0 || mapping.Size%responsePageSize != 0 || mapping.BaseHostVirtualAddress%responsePageSize != 0 {
			return fmt.Errorf("memory mapping %d is not aligned to response page size %d", index, responsePageSize)
		}
		guestEnd, ok := checkedAddUint64(mapping.Offset, mapping.Size)
		if !ok {
			return fmt.Errorf("memory mapping %d guest range overflows", index)
		}
		if guestEnd > memorySize {
			return fmt.Errorf("memory mapping %d extends outside memory size %d", index, memorySize)
		}
		hostEnd, ok := checkedAddUint64(mapping.BaseHostVirtualAddress, mapping.Size)
		if !ok {
			return fmt.Errorf("memory mapping %d host range overflows", index)
		}
		if mapping.BaseHostVirtualAddress > math.MaxInt64 || hostEnd > uint64(1)<<63 {
			return fmt.Errorf("memory mapping %d host range overflows signed proc memory offsets", index)
		}
		if index > 0 {
			if mapping.Offset <= previousOffset {
				return fmt.Errorf("memory mapping %d is unordered", index)
			}
			if mapping.Offset < previousEnd {
				return fmt.Errorf("memory mapping %d overlaps its predecessor", index)
			}
		}
		previousOffset, previousEnd = mapping.Offset, guestEnd
	}
	return nil
}

func validateSelectedMappings(ctx context.Context, mappings []MemoryMapping, memorySize, blockSize uint64, selector pageSelector) error {
	for offset := uint64(0); offset < memorySize; offset += blockSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		if selector(offset) == pageSkip {
			continue
		}
		if _, err := memoryMappingIndex(mappings, offset, blockSize); err != nil {
			return err
		}
	}
	return nil
}

func memoryMappingIndex(mappings []MemoryMapping, offset, length uint64) (int, error) {
	end, ok := checkedAddUint64(offset, length)
	if !ok {
		return 0, fmt.Errorf("guest memory range at offset %d overflows", offset)
	}
	for index, mapping := range mappings {
		if offset < mapping.Offset {
			break
		}
		if offset >= mapping.Offset && end <= mapping.Offset+mapping.Size {
			return index, nil
		}
	}
	return 0, fmt.Errorf("selected guest range [%d,%d) is gapped from memory mappings", offset, end)
}

func validateBitmap(name string, bitmap []uint64, memorySize, pageSize uint64) error {
	if memorySize == 0 || pageSize == 0 {
		return nil
	}
	pageCount := memorySize / pageSize
	wordCount := pageCount / 64
	if pageCount%64 != 0 {
		wordCount++
	}
	if uint64(len(bitmap)) > wordCount {
		return fmt.Errorf("%s bitmap contains words outside memory", name)
	}
	if len(bitmap) == 0 || pageCount%64 == 0 {
		return nil
	}
	lastWord := uint64(len(bitmap) - 1)
	if lastWord == wordCount-1 && bitmap[lastWord]>>(pageCount%64) != 0 {
		return fmt.Errorf("%s bitmap contains bits outside memory", name)
	}
	return nil
}

func bitmapBit(bitmap []uint64, page uint64) bool {
	word := page / 64
	return word < uint64(len(bitmap)) && bitmap[word]&(uint64(1)<<(page%64)) != 0
}

func isPowerOfTwo(value uint64) bool {
	return value != 0 && value&(value-1) == 0
}

func checkedAddUint64(left, right uint64) (uint64, bool) {
	result := left + right
	return result, result >= left
}

func isZeroMemoryPage(page []byte) bool {
	for _, value := range page {
		if value != 0 {
			return false
		}
	}
	return true
}
