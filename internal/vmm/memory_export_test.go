package vmm

import (
	"context"
	"strings"
	"testing"
)

const memoryExportTestPageSize = uint64(4096)

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

func TestExportMemoryByPageStateRejectsInvalidInput(t *testing.T) {
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
			err := ExportMemoryByPageState(context.Background(), 1234, 2*memoryExportTestPageSize, memoryExportTestPageSize, test.mappings, test.state, newRecordingPageSink())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ExportMemoryByPageState() error = %v, want %q", err, test.want)
			}
		})
	}
}
