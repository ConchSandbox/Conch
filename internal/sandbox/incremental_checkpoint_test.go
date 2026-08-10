package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/openeuler/Conch/internal/memsnap"
	"github.com/openeuler/Conch/internal/vmm/driver"
)

func TestIncrementalCaptureCreatesBaseEpochAndResumes(t *testing.T) {
	backend := &recordingIncrementalBackend{generation: 7}
	source := newIncrementalSource("cold", nil)
	result, err := newIncrementalCheckpointCapture(t.TempDir(), backend).Capture(context.Background(), RuntimeCaptureRequest{Source: source, PauseBefore: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(result.CleanupPath) })
	if !source.poisoned || source.pauses != 1 || source.resumes != 1 {
		t.Fatalf("source state: poisoned=%v pauses=%d resumes=%d", source.poisoned, source.pauses, source.resumes)
	}
	if result.MemoryFormat != incrementalMemoryFormat || result.Manifest == nil || result.Manifest.Layers[0] != "layers/0.mem" {
		t.Fatalf("capture result = %#v", result)
	}
	if backend.clearGeneration != nil {
		t.Fatalf("base capture unexpectedly cleared generation %v", backend.clearGeneration)
	}
	if !reflect.DeepEqual(backend.operations, []string{"state", "mappings", "page-state", "base", "manifest"}) {
		t.Fatalf("operations = %v", backend.operations)
	}
	for _, name := range []string{"state", "memory", memsnap.ManifestFileName, "layers/0.mem"} {
		if _, err := os.Stat(filepath.Join(result.MemRootPath, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestIncrementalCaptureCreatesDeltaAndClearsExactGenerationAfterManifest(t *testing.T) {
	parent := memsnap.Manifest{
		SchemaVersion: memsnap.SchemaVersion,
		MemorySize:    2 * memsnap.DefaultBlockSize,
		BlockSize:     memsnap.DefaultBlockSize,
		Layers:        []string{"layers/0.mem"},
		BuildMap:      []memsnap.BuildRange{{Offset: 0, Length: 2 * memsnap.DefaultBlockSize, LayerIndex: 0}},
	}
	backend := &recordingIncrementalBackend{generation: 42}
	source := newIncrementalSource("restored", &parent)
	result, err := newIncrementalCheckpointCapture(t.TempDir(), backend).Capture(context.Background(), RuntimeCaptureRequest{Source: source, PauseBefore: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(result.CleanupPath) })
	if result.Manifest == nil || !reflect.DeepEqual(result.Manifest.Layers, []string{"layers/0.mem", "layers/1.mem"}) {
		t.Fatalf("delta manifest = %#v", result.Manifest)
	}
	if backend.clearGeneration == nil || *backend.clearGeneration != 42 {
		t.Fatalf("cleared generation = %v", backend.clearGeneration)
	}
	if !reflect.DeepEqual(backend.operations, []string{"state", "mappings", "dirty", "delta", "manifest", "clear"}) {
		t.Fatalf("operations = %v", backend.operations)
	}
}

func TestIncrementalCaptureFailureAfterPausePoisonsButResumesSandbox(t *testing.T) {
	backend := &recordingIncrementalBackend{failAt: "manifest"}
	source := newIncrementalSource("cold", nil)
	root := t.TempDir()
	_, err := newIncrementalCheckpointCapture(root, backend).Capture(context.Background(), RuntimeCaptureRequest{Source: source, PauseBefore: true})
	if err == nil || !source.poisoned || source.resumes != 1 {
		t.Fatalf("failure=%v poisoned=%v resumes=%d", err, source.poisoned, source.resumes)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed epoch remained: entries=%v err=%v", entries, readErr)
	}
}

func TestIncrementalCapturePreflightFailureIsRetryable(t *testing.T) {
	source := newIncrementalSource("cold", nil)
	source.runtime.MemorySize = 0
	_, err := newIncrementalCheckpointCapture(t.TempDir(), &recordingIncrementalBackend{}).Capture(context.Background(), RuntimeCaptureRequest{Source: source, PauseBefore: true})
	if err == nil || source.poisoned || source.pauses != 0 {
		t.Fatalf("preflight error=%v poisoned=%v pauses=%d", err, source.poisoned, source.pauses)
	}
}

type incrementalSource struct {
	runtime  IncrementalMemoryRuntime
	poisoned bool
	pauses   int
	resumes  int
}

func newIncrementalSource(origin string, parent *memsnap.Manifest) *incrementalSource {
	return &incrementalSource{runtime: IncrementalMemoryRuntime{
		Origin: origin, ParentManifest: parent, MemorySize: 2 * memsnap.DefaultBlockSize,
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
func (fakeIncrementalAdapter) ClearMemoryDirtyBitmap(uint64) error { return nil }

type recordingIncrementalBackend struct {
	operations      []string
	generation      uint64
	clearGeneration *uint64
	failAt          string
}

func (backend *recordingIncrementalBackend) record(operation string) error {
	backend.operations = append(backend.operations, operation)
	if backend.failAt == operation {
		return errors.New("injected " + operation + " failure")
	}
	return nil
}

func (backend *recordingIncrementalBackend) CaptureExternalState(_ context.Context, _ IncrementalMemoryRuntime, root string) error {
	if err := backend.record("state"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "state"), []byte("state"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "memory"), []byte("metadata"), 0o600)
}
func (backend *recordingIncrementalBackend) QueryMappings(IncrementalMemoryRuntime) ([]driver.MemoryMapping, error) {
	return nil, backend.record("mappings")
}
func (backend *recordingIncrementalBackend) QueryPageState(IncrementalMemoryRuntime) (driver.MemoryPageState, error) {
	return driver.MemoryPageState{}, backend.record("page-state")
}
func (backend *recordingIncrementalBackend) QueryDirtyBitmap(IncrementalMemoryRuntime) (driver.MemoryDirtyBitmap, error) {
	return driver.MemoryDirtyBitmap{Generation: backend.generation}, backend.record("dirty")
}
func (backend *recordingIncrementalBackend) CreateBase(_ context.Context, runtime IncrementalMemoryRuntime, root string, _ []driver.MemoryMapping, _ driver.MemoryPageState) (memsnap.Manifest, error) {
	if err := backend.record("base"); err != nil {
		return memsnap.Manifest{}, err
	}
	return memsnap.CreateBaseLayer(root, runtime.MemorySize, runtime.BlockSize, func(sink memsnap.PageSink) error {
		for offset := uint64(0); offset < runtime.MemorySize; offset += runtime.BlockSize {
			if err := sink.WriteZeroPage(offset); err != nil {
				return err
			}
		}
		return nil
	})
}
func (backend *recordingIncrementalBackend) CreateDelta(_ context.Context, _ IncrementalMemoryRuntime, parent memsnap.Manifest, root string, _ []driver.MemoryMapping, _ driver.MemoryDirtyBitmap) (memsnap.Manifest, error) {
	if err := backend.record("delta"); err != nil {
		return memsnap.Manifest{}, err
	}
	return memsnap.CreateDeltaLayer(parent, root, func(sink memsnap.PageSink) error {
		return sink.WriteZeroPage(memsnap.DefaultBlockSize)
	})
}
func (backend *recordingIncrementalBackend) WriteManifest(path string, manifest memsnap.Manifest) error {
	if err := backend.record("manifest"); err != nil {
		return err
	}
	return memsnap.WriteManifestAtomic(path, manifest)
}
func (backend *recordingIncrementalBackend) ClearDirty(_ IncrementalMemoryRuntime, generation uint64) error {
	if err := backend.record("clear"); err != nil {
		return err
	}
	backend.clearGeneration = &generation
	return nil
}
