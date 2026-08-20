package sandbox

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/openeuler/Conch/internal/memsnap"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/vmm/driver"
)

func TestIncrementalCapturePreflightFailureIsRetryable(t *testing.T) {
	source := newIncrementalSource(nil)
	source.runtime.MemorySize = 0
	capture, err := NewIncrementalCheckpointCapture(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = capture.Capture(context.Background(), RuntimeCaptureRequest{Source: source, PauseBefore: true})
	if err == nil || source.poisoned || source.pauses != 0 {
		t.Fatalf("preflight error=%v poisoned=%v pauses=%d", err, source.poisoned, source.pauses)
	}
}

func TestIncrementalCaptureUsesFixedVMMStateDirectory(t *testing.T) {
	captureErr := errors.New("stop after recording VMM state path")
	adapter := &recordingIncrementalAdapter{err: captureErr}
	source := newIncrementalSource(nil)
	source.runtime.Adapter = adapter
	capture, err := NewIncrementalCheckpointCapture(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Capture(context.Background(), RuntimeCaptureRequest{Source: source}); !errors.Is(err, captureErr) {
		t.Fatalf("Capture() error = %v, want %v", err, captureErr)
	}
	if got := filepath.Base(adapter.path); got != common.VMMStateDir {
		t.Fatalf("VMM state directory = %q, want %q", got, common.VMMStateDir)
	}
}

type incrementalSource struct {
	runtime  IncrementalMemoryRuntime
	poisoned bool
	pauses   int
	resumes  int
}

func newIncrementalSource(parent *memsnap.Manifest) *incrementalSource {
	return &incrementalSource{runtime: IncrementalMemoryRuntime{
		ParentManifest: parent, MemorySize: 2 * memsnap.DefaultBlockSize,
		BlockSize: memsnap.DefaultBlockSize, PID: 123, Adapter: fakeIncrementalAdapter{},
	}}
}

func (source *incrementalSource) Pause(context.Context) error                  { source.pauses++; return nil }
func (source *incrementalSource) Resume(context.Context) error                 { source.resumes++; return nil }
func (source *incrementalSource) CreateVMMState(context.Context, string) error { return nil }
func (source *incrementalSource) MemoryBackingPath() string                    { return "" }
func (source *incrementalSource) MemorySizeMB() int64                          { return 1 }
func (source *incrementalSource) VMMName() string                              { return "stratovirt" }
func (source *incrementalSource) IncrementalMemoryRuntime() (IncrementalMemoryRuntime, error) {
	return source.runtime, nil
}
func (source *incrementalSource) SetCheckpointPoisoned(value bool) { source.poisoned = value }

type fakeIncrementalAdapter struct{}

func (fakeIncrementalAdapter) CreateExternalMemorySnapshot(string) error { return nil }
func (fakeIncrementalAdapter) QueryMemoryMappings() ([]driver.MemoryMapping, error) {
	return nil, nil
}
func (fakeIncrementalAdapter) QueryMemoryPageState() (driver.MemoryPageState, error) {
	return driver.MemoryPageState{}, nil
}
func (fakeIncrementalAdapter) QueryMemoryDirtyBitmap() (driver.MemoryDirtyBitmap, error) {
	return driver.MemoryDirtyBitmap{}, nil
}

type recordingIncrementalAdapter struct {
	fakeIncrementalAdapter
	path string
	err  error
}

func (adapter *recordingIncrementalAdapter) CreateExternalMemorySnapshot(path string) error {
	adapter.path = path
	return adapter.err
}
