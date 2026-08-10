package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openeuler/Conch/internal/memsnap"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/vmm"
	"github.com/openeuler/Conch/internal/vmm/driver"
)

const (
	incrementalMemoryFormat = "incremental-v1"
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

type IncrementalCheckpointCapture struct {
	tempRoot string
}

func NewIncrementalCheckpointCapture(tempRoot string) (*IncrementalCheckpointCapture, error) {
	if strings.TrimSpace(tempRoot) == "" {
		return nil, fmt.Errorf("incremental temporary root is required")
	}
	if !filepath.IsAbs(tempRoot) || filepath.Clean(tempRoot) != tempRoot {
		return nil, fmt.Errorf("incremental temporary root must be a clean absolute path")
	}
	return &IncrementalCheckpointCapture{tempRoot: tempRoot}, nil
}

func (capture *IncrementalCheckpointCapture) Capture(ctx context.Context, req RuntimeCaptureRequest) (_ CapturedBootComponents, retErr error) {
	source, ok := req.Source.(IncrementalRuntimeCaptureSource)
	if !ok || source == nil {
		return CapturedBootComponents{}, fmt.Errorf("incremental checkpoint source is required")
	}
	if capture == nil {
		return CapturedBootComponents{}, fmt.Errorf("incremental checkpoint capture is not configured")
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
	// From this point, a failure may leave dirty-page tracking reset without a
	// published checkpoint. The runtime may resume, but subsequent checkpoints
	// must be rejected.
	source.SetCheckpointPoisoned(true)
	defer func() {
		if !pausedByCapture {
			return
		}
		if resumeErr := source.Resume(context.WithoutCancel(ctx)); resumeErr != nil {
			retErr = errors.Join(retErr, checkpointResumeError(resumeErr))
		}
	}()

	vmmStatePath := filepath.Join(epochRoot, common.VMMStateDir)
	if err := os.MkdirAll(vmmStatePath, 0o700); err != nil {
		return CapturedBootComponents{}, fmt.Errorf("create VMM state directory: %w", err)
	}
	if err := runtime.Adapter.CreateExternalMemorySnapshot(vmmStatePath); err != nil {
		return CapturedBootComponents{}, fmt.Errorf("capture external VMM state: %w", err)
	}
	mappings, err := runtime.Adapter.QueryMemoryMappings()
	if err != nil {
		return CapturedBootComponents{}, fmt.Errorf("query memory mappings: %w", err)
	}
	var manifest memsnap.Manifest
	if runtime.ParentManifest == nil {
		pageState, err := runtime.Adapter.QueryMemoryPageState()
		if err != nil {
			return CapturedBootComponents{}, fmt.Errorf("query memory page state: %w", err)
		}
		manifest, err = memsnap.CreateBaseLayer(epochRoot, runtime.MemorySize, runtime.BlockSize, func(sink memsnap.PageSink) error {
			return vmm.ExportMemoryByPageState(ctx, runtime.PID, runtime.MemorySize, runtime.BlockSize, mappings, pageState, sink)
		})
		if err != nil {
			return CapturedBootComponents{}, fmt.Errorf("create base memory layer: %w", err)
		}
	} else {
		dirty, err := runtime.Adapter.QueryMemoryDirtyBitmap()
		if err != nil {
			return CapturedBootComponents{}, fmt.Errorf("query memory dirty bitmap: %w", err)
		}
		manifest, err = memsnap.CreateDeltaLayer(*runtime.ParentManifest, epochRoot, func(sink memsnap.PageSink) error {
			return vmm.ExportMemoryByDirtyBitmap(ctx, runtime.PID, runtime.MemorySize, runtime.BlockSize, mappings, dirty, sink)
		})
		if err != nil {
			return CapturedBootComponents{}, fmt.Errorf("create delta memory layer: %w", err)
		}
	}
	if err := memsnap.WriteManifestAtomic(filepath.Join(epochRoot, memsnap.ManifestFileName), manifest); err != nil {
		return CapturedBootComponents{}, fmt.Errorf("write memory manifest: %w", err)
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
