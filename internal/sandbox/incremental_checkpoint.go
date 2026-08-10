package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openeuler/Conch/internal/memsnap"
	"github.com/openeuler/Conch/internal/vmm"
	"github.com/openeuler/Conch/internal/vmm/driver"
)

const (
	incrementalMemoryFormat = "incremental-v1"
	defaultIncrementalRoot  = "/var/lib/conch/incremental"
)

type IncrementalMemoryRuntime struct {
	Origin         string
	ParentManifest *memsnap.Manifest
	MemorySize     uint64
	BlockSize      uint64
	PID            int
	Adapter        driver.IncrementalMemoryAdapter
}

type IncrementalRuntimeCaptureSource interface {
	RuntimeCaptureSource
	IncrementalMemoryRuntime() (IncrementalMemoryRuntime, error)
	SetCheckpointPoisoned(bool)
}

type incrementalCaptureBackend interface {
	CaptureExternalState(context.Context, IncrementalMemoryRuntime, string) error
	QueryMappings(IncrementalMemoryRuntime) ([]driver.MemoryMapping, error)
	QueryPageState(IncrementalMemoryRuntime) (driver.MemoryPageState, error)
	QueryDirtyBitmap(IncrementalMemoryRuntime) (driver.MemoryDirtyBitmap, error)
	CreateBase(context.Context, IncrementalMemoryRuntime, string, []driver.MemoryMapping, driver.MemoryPageState) (memsnap.Manifest, error)
	CreateDelta(context.Context, IncrementalMemoryRuntime, memsnap.Manifest, string, []driver.MemoryMapping, driver.MemoryDirtyBitmap) (memsnap.Manifest, error)
	WriteManifest(string, memsnap.Manifest) error
	ClearDirty(IncrementalMemoryRuntime, uint64) error
}

type IncrementalCheckpointCapture struct {
	tempRoot string
	backend  incrementalCaptureBackend
}

func NewIncrementalCheckpointCapture(tempRoot string) (*IncrementalCheckpointCapture, error) {
	if strings.TrimSpace(tempRoot) == "" {
		tempRoot = defaultIncrementalRoot
	}
	if !filepath.IsAbs(tempRoot) || filepath.Clean(tempRoot) != tempRoot {
		return nil, fmt.Errorf("incremental temporary root must be a clean absolute path")
	}
	return &IncrementalCheckpointCapture{tempRoot: tempRoot, backend: osIncrementalBackend{}}, nil
}

func newIncrementalCheckpointCapture(tempRoot string, backend incrementalCaptureBackend) *IncrementalCheckpointCapture {
	return &IncrementalCheckpointCapture{tempRoot: tempRoot, backend: backend}
}

func (capture *IncrementalCheckpointCapture) Capture(ctx context.Context, req RuntimeCaptureRequest) (_ CapturedBootComponents, retErr error) {
	source, ok := req.Source.(IncrementalRuntimeCaptureSource)
	if !ok || source == nil {
		return CapturedBootComponents{}, fmt.Errorf("incremental checkpoint source is required")
	}
	if capture == nil || capture.backend == nil {
		return CapturedBootComponents{}, fmt.Errorf("incremental checkpoint backend is not configured")
	}
	runtime, err := source.IncrementalMemoryRuntime()
	if err != nil {
		return CapturedBootComponents{}, err
	}
	if err := validateIncrementalRuntime(runtime); err != nil {
		return CapturedBootComponents{}, err
	}
	if err := os.MkdirAll(capture.tempRoot, 0o700); err != nil {
		return CapturedBootComponents{}, fmt.Errorf("create incremental temporary root: %w", err)
	}
	epochRoot, err := os.MkdirTemp(capture.tempRoot, "epoch-")
	if err != nil {
		return CapturedBootComponents{}, fmt.Errorf("create checkpoint epoch: %w", err)
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(epochRoot))
		}
	}()

	pausedByCapture := false
	if req.PauseBefore {
		if err := source.Pause(ctx); err != nil {
			return CapturedBootComponents{}, fmt.Errorf("pause sandbox for incremental checkpoint: %w", err)
		}
		pausedByCapture = true
	}
	// From this point, a failure prevents reliable reconstruction of the dirty-page
	// generation. The runtime may resume, but subsequent checkpoints must be rejected.
	source.SetCheckpointPoisoned(true)
	defer func() {
		if !pausedByCapture {
			return
		}
		if resumeErr := source.Resume(context.WithoutCancel(ctx)); resumeErr != nil {
			retErr = errors.Join(retErr, checkpointResumeError(resumeErr))
		}
	}()

	if err := capture.backend.CaptureExternalState(ctx, runtime, epochRoot); err != nil {
		return CapturedBootComponents{}, fmt.Errorf("capture external VMM state: %w", err)
	}
	mappings, err := capture.backend.QueryMappings(runtime)
	if err != nil {
		return CapturedBootComponents{}, fmt.Errorf("query memory mappings: %w", err)
	}
	var manifest memsnap.Manifest
	var generation uint64
	clearDirty := false
	if runtime.ParentManifest == nil {
		pageState, err := capture.backend.QueryPageState(runtime)
		if err != nil {
			return CapturedBootComponents{}, fmt.Errorf("query memory page state: %w", err)
		}
		manifest, err = capture.backend.CreateBase(ctx, runtime, epochRoot, mappings, pageState)
		if err != nil {
			return CapturedBootComponents{}, fmt.Errorf("create base memory layer: %w", err)
		}
	} else {
		dirty, err := capture.backend.QueryDirtyBitmap(runtime)
		if err != nil {
			return CapturedBootComponents{}, fmt.Errorf("query memory dirty bitmap: %w", err)
		}
		generation = dirty.Generation
		clearDirty = true
		manifest, err = capture.backend.CreateDelta(ctx, runtime, *runtime.ParentManifest, epochRoot, mappings, dirty)
		if err != nil {
			return CapturedBootComponents{}, fmt.Errorf("create delta memory layer: %w", err)
		}
	}
	if err := capture.backend.WriteManifest(filepath.Join(epochRoot, memsnap.ManifestFileName), manifest); err != nil {
		return CapturedBootComponents{}, fmt.Errorf("write memory manifest: %w", err)
	}
	if clearDirty {
		if err := capture.backend.ClearDirty(runtime, generation); err != nil {
			return CapturedBootComponents{}, fmt.Errorf("clear memory dirty bitmap generation %d: %w", generation, err)
		}
	}

	memorySizeMB := source.MemorySizeMB()
	if memorySizeMB <= 0 {
		return CapturedBootComponents{}, fmt.Errorf("checkpoint memory size must be positive")
	}
	return CapturedBootComponents{
		MemRootPath:  epochRoot,
		CleanupPath:  epochRoot,
		VMMName:      vmm.StratovirtName,
		MemorySizeMB: memorySizeMB,
		MemoryFormat: incrementalMemoryFormat,
		Manifest:     &manifest,
	}, nil
}

func validateIncrementalRuntime(runtime IncrementalMemoryRuntime) error {
	if runtime.Origin != "cold" && runtime.Origin != "restored" {
		return fmt.Errorf("invalid incremental memory origin %q", runtime.Origin)
	}
	if runtime.MemorySize == 0 || runtime.BlockSize == 0 || runtime.MemorySize%runtime.BlockSize != 0 {
		return fmt.Errorf("invalid incremental memory geometry")
	}
	if runtime.PID <= 0 || runtime.Adapter == nil {
		return fmt.Errorf("incremental VMM capture is unavailable")
	}
	if runtime.Origin == "cold" && runtime.ParentManifest != nil {
		return fmt.Errorf("cold runtime unexpectedly has a parent manifest")
	}
	if runtime.Origin == "restored" && runtime.ParentManifest == nil {
		return fmt.Errorf("restored runtime is missing a parent manifest")
	}
	return nil
}

type osIncrementalBackend struct{}

func (osIncrementalBackend) CaptureExternalState(_ context.Context, runtime IncrementalMemoryRuntime, root string) error {
	return runtime.Adapter.CreateExternalMemorySnapshot(root)
}
func (osIncrementalBackend) QueryMappings(runtime IncrementalMemoryRuntime) ([]driver.MemoryMapping, error) {
	return runtime.Adapter.QueryMemoryMappings()
}
func (osIncrementalBackend) QueryPageState(runtime IncrementalMemoryRuntime) (driver.MemoryPageState, error) {
	return runtime.Adapter.QueryMemoryPageState()
}
func (osIncrementalBackend) QueryDirtyBitmap(runtime IncrementalMemoryRuntime) (driver.MemoryDirtyBitmap, error) {
	return runtime.Adapter.QueryMemoryDirtyBitmap()
}
func (osIncrementalBackend) CreateBase(ctx context.Context, runtime IncrementalMemoryRuntime, root string, mappings []driver.MemoryMapping, state driver.MemoryPageState) (memsnap.Manifest, error) {
	return memsnap.CreateBaseLayer(root, runtime.MemorySize, runtime.BlockSize, func(sink memsnap.PageSink) error {
		return vmm.ExportMemoryByPageState(ctx, runtime.PID, runtime.MemorySize, runtime.BlockSize, mappings, state, sink)
	})
}
func (osIncrementalBackend) CreateDelta(ctx context.Context, runtime IncrementalMemoryRuntime, parent memsnap.Manifest, root string, mappings []driver.MemoryMapping, dirty driver.MemoryDirtyBitmap) (memsnap.Manifest, error) {
	return memsnap.CreateDeltaLayer(parent, root, func(sink memsnap.PageSink) error {
		return vmm.ExportMemoryByDirtyBitmap(ctx, runtime.PID, runtime.MemorySize, runtime.BlockSize, mappings, dirty, sink)
	})
}
func (osIncrementalBackend) WriteManifest(path string, manifest memsnap.Manifest) error {
	return memsnap.WriteManifestAtomic(path, manifest)
}
func (osIncrementalBackend) ClearDirty(runtime IncrementalMemoryRuntime, generation uint64) error {
	return runtime.Adapter.ClearMemoryDirtyBitmap(generation)
}
