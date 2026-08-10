package vmm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const memoryExportTestPageSize = uint64(4096)

type recordedMemoryRead struct {
	offset int64
	length int
}

type observedProcessMemory struct {
	file  *os.File
	reads []recordedMemoryRead
}

func (memory *observedProcessMemory) ReadAt(data []byte, offset int64) (int, error) {
	memory.reads = append(memory.reads, recordedMemoryRead{offset: offset, length: len(data)})
	return memory.file.ReadAt(data, offset)
}

func (memory *observedProcessMemory) Close() error { return memory.file.Close() }

type recordingPageSink struct {
	pages map[uint64][]byte
	zeros map[uint64]bool
}

func newRecordingPageSink() *recordingPageSink {
	return &recordingPageSink{pages: make(map[uint64][]byte), zeros: make(map[uint64]bool)}
}

func (sink *recordingPageSink) WritePage(offset uint64, page []byte) error {
	sink.pages[offset] = append([]byte(nil), page...)
	return nil
}

func (sink *recordingPageSink) WriteZeroPage(offset uint64) error {
	sink.zeros[offset] = true
	return nil
}

func filledMemoryExportPage(value byte) []byte {
	return bytes.Repeat([]byte{value}, int(memoryExportTestPageSize))
}

func newObservedProcessMemory(t *testing.T, size int64, writes map[int64][]byte) *observedProcessMemory {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(t.TempDir(), "proc-mem"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for offset, data := range writes {
		if _, err := file.WriteAt(data, offset); err != nil {
			t.Fatal(err)
		}
	}
	return &observedProcessMemory{file: file}
}

func TestMemoryExporterPageStateTranslatesMappingsAndPreservesZeroOwnership(t *testing.T) {
	const secondHVA = int64(8 * memoryExportTestPageSize)
	memory := newObservedProcessMemory(t, secondHVA+2*int64(memoryExportTestPageSize), map[int64][]byte{
		int64(memoryExportTestPageSize):             filledMemoryExportPage(0x11),
		secondHVA + int64(memoryExportTestPageSize): filledMemoryExportPage(0x44),
	})
	exporter := newMemoryExporter(func(pid int) (processMemory, error) {
		if pid != 1234 {
			return nil, fmt.Errorf("pid = %d", pid)
		}
		return memory, nil
	}, 2*memoryExportTestPageSize)
	sink := newRecordingPageSink()
	mappings := []MemoryMapping{
		{BaseHostVirtualAddress: memoryExportTestPageSize, Size: 2 * memoryExportTestPageSize, Offset: 0, PageSize: memoryExportTestPageSize},
		{BaseHostVirtualAddress: uint64(secondHVA), Size: 2 * memoryExportTestPageSize, Offset: 2 * memoryExportTestPageSize, PageSize: memoryExportTestPageSize},
	}
	state := MemoryPageState{Resident: []uint64{0b1011}, Empty: []uint64{0b0010}, PageSize: memoryExportTestPageSize}

	if err := exporter.ExportPageState(context.Background(), 1234, 4*memoryExportTestPageSize, memoryExportTestPageSize, mappings, state, sink); err != nil {
		t.Fatalf("ExportPageState() error = %v", err)
	}
	if !bytes.Equal(sink.pages[0], filledMemoryExportPage(0x11)) {
		t.Fatal("guest page 0 was not read from the first HVA mapping")
	}
	if !sink.zeros[memoryExportTestPageSize] || !sink.zeros[2*memoryExportTestPageSize] {
		t.Fatalf("zero ownership = %#v, want empty and non-resident pages", sink.zeros)
	}
	if !bytes.Equal(sink.pages[3*memoryExportTestPageSize], filledMemoryExportPage(0x44)) {
		t.Fatal("guest page 3 was not read from the second HVA mapping")
	}
	wantReads := []recordedMemoryRead{
		{offset: int64(memoryExportTestPageSize), length: int(memoryExportTestPageSize)},
		{offset: secondHVA + int64(memoryExportTestPageSize), length: int(memoryExportTestPageSize)},
	}
	if fmt.Sprint(memory.reads) != fmt.Sprint(wantReads) {
		t.Fatalf("reads = %#v, want %#v", memory.reads, wantReads)
	}
}

func TestMemoryExporterDirtyBitmapExportsOnlyDirtyPagesIncludingZeroPage(t *testing.T) {
	memory := newObservedProcessMemory(t, int64(3*memoryExportTestPageSize), map[int64][]byte{
		int64(memoryExportTestPageSize): filledMemoryExportPage(0x5a),
	})
	exporter := newMemoryExporter(func(int) (processMemory, error) { return memory, nil }, memoryExportTestPageSize)
	sink := newRecordingPageSink()
	mappings := []MemoryMapping{{Size: 3 * memoryExportTestPageSize, PageSize: memoryExportTestPageSize}}
	dirty := MemoryDirtyBitmap{Bitmap: []uint64{0b110}, PageSize: memoryExportTestPageSize}

	if err := exporter.ExportDirtyBitmap(context.Background(), 1234, 3*memoryExportTestPageSize, memoryExportTestPageSize, mappings, dirty, sink); err != nil {
		t.Fatalf("ExportDirtyBitmap() error = %v", err)
	}
	if !bytes.Equal(sink.pages[memoryExportTestPageSize], filledMemoryExportPage(0x5a)) {
		t.Fatal("dirty nonzero page was not exported")
	}
	if !sink.zeros[2*memoryExportTestPageSize] {
		t.Fatal("dirty zero page did not retain ownership in the delta layer")
	}
	if len(sink.pages)+len(sink.zeros) != 2 {
		t.Fatalf("clean pages were exported: pages=%v zeros=%v", sink.pages, sink.zeros)
	}
}

func TestMemoryExporterRejectsInvalidInputBeforeOpeningProcessMemory(t *testing.T) {
	tests := []struct {
		name     string
		mappings []MemoryMapping
		state    MemoryPageState
		want     string
	}{
		{name: "empty mappings", state: MemoryPageState{Resident: []uint64{1}, PageSize: memoryExportTestPageSize}, want: "mappings"},
		{name: "mapping gap selected", mappings: []MemoryMapping{{Size: memoryExportTestPageSize, PageSize: memoryExportTestPageSize}}, state: MemoryPageState{Resident: []uint64{0b11}, PageSize: memoryExportTestPageSize}, want: "mapping"},
		{name: "empty without resident", mappings: []MemoryMapping{{Size: 2 * memoryExportTestPageSize, PageSize: memoryExportTestPageSize}}, state: MemoryPageState{Empty: []uint64{1}, PageSize: memoryExportTestPageSize}, want: "resident"},
		{name: "bitmap outside memory", mappings: []MemoryMapping{{Size: 2 * memoryExportTestPageSize, PageSize: memoryExportTestPageSize}}, state: MemoryPageState{Resident: []uint64{1 << 2}, PageSize: memoryExportTestPageSize}, want: "outside memory"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opened := false
			exporter := newMemoryExporter(func(int) (processMemory, error) {
				opened = true
				return nil, errors.New("unexpected open")
			}, memoryExportTestPageSize)
			err := exporter.ExportPageState(context.Background(), 1234, 2*memoryExportTestPageSize, memoryExportTestPageSize, test.mappings, test.state, newRecordingPageSink())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ExportPageState() error = %v, want %q", err, test.want)
			}
			if opened {
				t.Fatal("invalid input opened process memory")
			}
		})
	}
}

func TestMemoryExporterRejectsShortProcessMemoryRead(t *testing.T) {
	memory := newObservedProcessMemory(t, int64(memoryExportTestPageSize/2), nil)
	exporter := newMemoryExporter(func(int) (processMemory, error) { return memory, nil }, memoryExportTestPageSize)
	err := exporter.ExportDirtyBitmap(context.Background(), 1234, memoryExportTestPageSize, memoryExportTestPageSize,
		[]MemoryMapping{{Size: memoryExportTestPageSize, PageSize: memoryExportTestPageSize}},
		MemoryDirtyBitmap{Bitmap: []uint64{1}, PageSize: memoryExportTestPageSize}, newRecordingPageSink())
	if err == nil || (!strings.Contains(err.Error(), "short") && !errors.Is(err, io.EOF)) {
		t.Fatalf("ExportDirtyBitmap() error = %v, want short read", err)
	}
}
