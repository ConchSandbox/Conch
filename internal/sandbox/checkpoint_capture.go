package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/openeuler/Conch/internal/memsnap"
	"github.com/openeuler/Conch/internal/vmm"
	"golang.org/x/sys/unix"
)

const (
	capturedMemoryFileName = "mem.img"
	capturedSnapshotDir    = "conch/snapshot"
	checkpointTempDir      = "/tmp"
)

// ErrCheckpointResume marks a failure to restore a sandbox that this capture
// adapter successfully paused. Callers can use it to retain suspended runtime
// state rather than reporting the sandbox as running after a failed rollback.
var ErrCheckpointResume = errors.New("failed to restore running sandbox after checkpoint capture")

// CheckpointCapture captures a complete, publishable view of a sandbox's boot
// components without changing the sandbox's lifecycle state.
type CheckpointCapture interface {
	Capture(context.Context, RuntimeCaptureRequest) (CapturedBootComponents, error)
}

// RuntimeCaptureSource is the runtime-facing seam used by checkpoint capture.
type RuntimeCaptureSource interface {
	Pause(context.Context) error
	Resume(context.Context) error
	CreateVMMState(context.Context, string) error
	MemoryBackingPath() string
	MemorySizeMB() int64
	VMMName() string
}

// CheckpointArtifactCapturer owns the VMM-specific contents of one checkpoint
// staging root. Lifecycle transitions and workspace cleanup remain generic.
type CheckpointArtifactCapturer interface {
	CaptureCheckpointArtifacts(context.Context, RuntimeCaptureSource, string) error
}

// RuntimeCaptureRequest describes the runtime and its lifecycle state at the
// checkpoint boundary. PauseBefore is true for a running sandbox and false for
// a sandbox that was already suspended by its caller.
type RuntimeCaptureRequest struct {
	Source      RuntimeCaptureSource
	PauseBefore bool
}

// CapturedBootComponents contains the mutable component inputs for Boot Index
// publication. MemRootPath is owned by the caller after success. Its contents
// are VMM-specific; immutable rootfs and sandbox components are reused from the
// source Boot Index by the publisher.
type CapturedBootComponents struct {
	MemRootPath  string
	CleanupPath  string
	VMMName      string
	MemorySizeMB int64
	MemoryFormat string
	Manifest     *memsnap.Manifest
}

// FullCheckpointCapture is the default full-checkpoint implementation used
// before incremental epoch capture is introduced.
type FullCheckpointCapture struct{}

var _ CheckpointCapture = (*FullCheckpointCapture)(nil)
var _ RuntimeCaptureSource = (*Sandbox)(nil)

// NewFullCheckpointCapture returns the default full-checkpoint capture adapter.
func NewFullCheckpointCapture() *FullCheckpointCapture {
	return &FullCheckpointCapture{}
}

// Capture creates a self-contained memory/VMM component while the guest is
// paused. A sandbox that was running is restored before Capture returns,
// including every error path. A sandbox that was already suspended is never
// resumed by this adapter.
func (c *FullCheckpointCapture) Capture(ctx context.Context, req RuntimeCaptureRequest) (_ CapturedBootComponents, retErr error) {
	if req.Source == nil {
		return CapturedBootComponents{}, fmt.Errorf("checkpoint capture source is required")
	}

	vmmName := strings.TrimSpace(req.Source.VMMName())
	if vmmName == "" {
		return CapturedBootComponents{}, fmt.Errorf("checkpoint VMM name is required")
	}
	artifactCapturer, err := checkpointArtifactCapturerFor(vmmName)
	if err != nil {
		return CapturedBootComponents{}, err
	}
	memorySizeMB := req.Source.MemorySizeMB()
	if memorySizeMB <= 0 {
		return CapturedBootComponents{}, fmt.Errorf("checkpoint memory size must be positive, got %d MB", memorySizeMB)
	}
	memRoot, err := os.MkdirTemp(checkpointTempDir, "conch-full-capture-*")
	if err != nil {
		return CapturedBootComponents{}, fmt.Errorf("create checkpoint staging directory: %w", err)
	}

	// Cleanup runs after lifecycle restoration so a VMM implementation has
	// access to its capture files until Resume has completed.
	defer func() {
		if retErr == nil {
			return
		}
		if cleanupErr := os.RemoveAll(memRoot); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove checkpoint staging directory: %w", cleanupErr))
		}
	}()

	resumePending := false
	resumeAttempted := false
	defer func() {
		if !resumePending || resumeAttempted {
			return
		}
		resumeAttempted = true
		if resumeErr := req.Source.Resume(context.WithoutCancel(ctx)); resumeErr != nil {
			retErr = errors.Join(retErr, checkpointResumeError(resumeErr))
		}
	}()

	if req.PauseBefore {
		if err := req.Source.Pause(ctx); err != nil {
			return CapturedBootComponents{}, fmt.Errorf("pause sandbox for checkpoint capture: %w", err)
		}
		resumePending = true
	}

	if err := artifactCapturer.CaptureCheckpointArtifacts(ctx, req.Source, memRoot); err != nil {
		return CapturedBootComponents{}, fmt.Errorf("capture %s checkpoint artifacts: %w", vmmName, err)
	}

	if req.PauseBefore {
		resumeAttempted = true
		if err := req.Source.Resume(context.WithoutCancel(ctx)); err != nil {
			return CapturedBootComponents{}, checkpointResumeError(err)
		}
	}

	return CapturedBootComponents{
		MemRootPath:  memRoot,
		CleanupPath:  memRoot,
		VMMName:      vmmName,
		MemorySizeMB: memorySizeMB,
		MemoryFormat: func() string {
			if vmmName == vmm.StratovirtName {
				return "full-v1"
			}
			return ""
		}(),
	}, nil
}

func checkpointArtifactCapturerFor(vmmName string) (CheckpointArtifactCapturer, error) {
	switch vmmName {
	case vmm.CloudHypervisorName:
		return cloudHypervisorCheckpointCapturer{}, nil
	case vmm.StratovirtName:
		return stratovirtCheckpointCapturer{}, nil
	default:
		return nil, fmt.Errorf("unsupported checkpoint VMM %q", vmmName)
	}
}

type cloudHypervisorCheckpointCapturer struct{}

func (cloudHypervisorCheckpointCapturer) CaptureCheckpointArtifacts(ctx context.Context, source RuntimeCaptureSource, stagingRoot string) error {
	memoryPath := strings.TrimSpace(source.MemoryBackingPath())
	if memoryPath == "" {
		return fmt.Errorf("checkpoint memory backing path is required")
	}
	snapshotDir := filepath.Join(stagingRoot, capturedSnapshotDir)
	if err := os.MkdirAll(snapshotDir, 0o750); err != nil {
		return fmt.Errorf("create VMM state directory: %w", err)
	}
	if err := source.CreateVMMState(ctx, snapshotDir); err != nil {
		return fmt.Errorf("capture VMM state: %w", err)
	}
	if err := requireRegularFileInTree(snapshotDir); err != nil {
		return fmt.Errorf("validate Cloud Hypervisor snapshot: %w", err)
	}

	memFile := filepath.Join(stagingRoot, capturedMemoryFileName)
	if err := copyMemoryFile(ctx, memoryPath, memFile); err != nil {
		return fmt.Errorf("capture memory backing %s: %w", memoryPath, err)
	}
	return nil
}

type stratovirtCheckpointCapturer struct{}

func (stratovirtCheckpointCapturer) CaptureCheckpointArtifacts(ctx context.Context, source RuntimeCaptureSource, stagingRoot string) error {
	if err := source.CreateVMMState(ctx, stagingRoot); err != nil {
		return fmt.Errorf("capture VMM state: %w", err)
	}
	if err := validateStratovirtSnapshot(stagingRoot); err != nil {
		return fmt.Errorf("validate StratoVirt snapshot: %w", err)
	}
	return nil
}

func requireRegularFileInTree(root string) error {
	found := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			found = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("snapshot contains no regular files")
	}
	return nil
}

func validateStratovirtSnapshot(snapshotDir string) error {
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		return err
	}
	want := map[string]struct{}{"state": {}, "memory": {}}
	for _, entry := range entries {
		if _, ok := want[entry.Name()]; !ok {
			return fmt.Errorf("unexpected artifact %q", entry.Name())
		}
		info, err := os.Lstat(filepath.Join(snapshotDir, entry.Name()))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact %q is not a regular file", entry.Name())
		}
		delete(want, entry.Name())
	}
	if len(want) != 0 {
		missing := make([]string, 0, len(want))
		for name := range want {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return fmt.Errorf("missing required artifacts: %s", strings.Join(missing, ", "))
	}
	return nil
}

func checkpointResumeError(err error) error {
	return fmt.Errorf("%w: %w", ErrCheckpointResume, err)
}

func copyMemoryFile(ctx context.Context, srcPath, dstPath string) (retErr error) {
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() {
		if closeErr := src.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close source: %w", closeErr))
		}
	}()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer func() {
		if closeErr := dst.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close destination: %w", closeErr))
		}
	}()

	if err := dst.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("preserve destination mode: %w", err)
	}
	if err := dst.Truncate(info.Size()); err != nil {
		return fmt.Errorf("size destination: %w", err)
	}

	if info.Size() > 0 {
		if err := copyFileRangeWithFallback(ctx, src, dst, info.Size()); err != nil {
			return err
		}
	}
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("sync destination: %w", err)
	}
	return nil
}

const maxCopyFileRangeChunk = 1 << 30 // 1 GiB

func copyFileRangeWithFallback(ctx context.Context, src, dst *os.File, size int64) error {
	var offIn, offOut int64
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := remaining
		if chunk > maxCopyFileRangeChunk {
			chunk = maxCopyFileRangeChunk
		}
		n, err := unix.CopyFileRange(int(src.Fd()), &offIn, int(dst.Fd()), &offOut, int(chunk), 0)
		if err != nil {
			if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) {
				return copyDenseWithContext(ctx, src, dst, size)
			}
			return fmt.Errorf("copy_file_range: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("copy_file_range copied 0 bytes with %d remaining", remaining)
		}
		remaining -= int64(n)
	}
	return nil
}

const copyBufferSize = 1024 * 1024

func copyDenseWithContext(ctx context.Context, src, dst *os.File, size int64) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind source: %w", err)
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind destination: %w", err)
	}
	buffer := make([]byte, copyBufferSize)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := int64(len(buffer))
		if remaining < chunk {
			chunk = remaining
		}
		n, readErr := io.ReadFull(src, buffer[:chunk])
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			if writeErr != nil {
				return fmt.Errorf("write destination: %w", writeErr)
			}
			if written != n {
				return io.ErrShortWrite
			}
			remaining -= int64(n)
		}
		if readErr != nil {
			return fmt.Errorf("read source: %w", readErr)
		}
	}
	return nil
}
