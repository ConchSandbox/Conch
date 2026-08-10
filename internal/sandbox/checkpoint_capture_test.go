package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

func TestFullCheckpointCaptureRunningCopiesCompleteMemoryComponent(t *testing.T) {
	tempRoot := t.TempDir()
	memoryPath, expectedMemory := writeSparseMemoryBacking(t, tempRoot)
	source := &fakeRuntimeCaptureSource{
		memoryPath:        memoryPath,
		vmmName:           "cloud-hypervisor",
		memorySizeMB:      512,
		checkCopyOnResume: true,
		expectedMemory:    expectedMemory,
	}

	captured, err := NewFullCheckpointCapture().Capture(context.Background(), RuntimeCaptureRequest{
		Source:      source,
		PauseBefore: true,
	})
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(captured.MemRootPath) })
	if got := filepath.Dir(captured.MemRootPath); got != checkpointTempDir {
		t.Fatalf("capture temp dir = %q, want parent %q", captured.MemRootPath, checkpointTempDir)
	}

	if got, want := strings.Join(source.events, ","), "pause,capture,resume"; got != want {
		t.Fatalf("capture event order = %q, want %q", got, want)
	}
	if source.resumeCheckErr != nil {
		t.Fatalf("memory copy was not complete before resume: %v", source.resumeCheckErr)
	}
	if captured.VMMName != source.vmmName {
		t.Fatalf("VMMName = %q, want %q", captured.VMMName, source.vmmName)
	}
	if captured.MemorySizeMB != source.memorySizeMB {
		t.Fatalf("MemorySizeMB = %d, want %d", captured.MemorySizeMB, source.memorySizeMB)
	}
	gotMemory, err := os.ReadFile(filepath.Join(captured.MemRootPath, capturedMemoryFileName))
	if err != nil {
		t.Fatalf("read captured memory: %v", err)
	}
	if !reflect.DeepEqual(gotMemory, expectedMemory) {
		t.Fatal("captured memory bytes differ from external backing")
	}
	info, err := os.Stat(filepath.Join(captured.MemRootPath, capturedMemoryFileName))
	if err != nil {
		t.Fatalf("stat captured memory: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o640); got != want {
		t.Fatalf("captured memory mode = %o, want %o", got, want)
	}

	statePath := filepath.Join(captured.MemRootPath, common.VMMStateDir, "state.bin")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read captured VMM state: %v", err)
	}
	if string(state) != "vmm-state" {
		t.Fatalf("captured VMM state = %q", state)
	}
}

func TestFullCheckpointCaptureStratovirtDoesNotRequireMemoryBacking(t *testing.T) {
	source := &fakeRuntimeCaptureSource{
		memorySizeMB: 256,
		vmmName:      "stratovirt",
		resumeErr:    errors.New("resume must not be called"),
	}

	captured, err := NewFullCheckpointCapture().Capture(context.Background(), RuntimeCaptureRequest{
		Source:      source,
		PauseBefore: false,
	})
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(captured.MemRootPath) })
	if got, want := strings.Join(source.events, ","), "capture"; got != want {
		t.Fatalf("capture events = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(captured.MemRootPath, capturedMemoryFileName)); !os.IsNotExist(err) {
		t.Fatalf("StratoVirt capture unexpectedly contains mem.img: %v", err)
	}
	if captured.MemoryFormat != "full" || captured.CleanupPath != captured.MemRootPath {
		t.Fatalf("StratoVirt capture metadata = %#v", captured)
	}
	for _, name := range []string{"state", "memory"} {
		if _, err := os.Stat(filepath.Join(captured.MemRootPath, common.VMMStateDir, name)); err != nil {
			t.Fatalf("StratoVirt artifact %s is unavailable: %v", name, err)
		}
	}
	entries, err := os.ReadDir(captured.MemRootPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != common.VMMStateDir || !entries[0].IsDir() {
		t.Fatalf("StratoVirt snapshot entries = %#v, want only %s/", entries, common.VMMStateDir)
	}
}

func TestFullCheckpointCaptureRejectsIncompleteStratovirtArtifacts(t *testing.T) {
	tests := []struct {
		name      string
		artifacts map[string]string
		wantError string
	}{
		{name: "missing state", artifacts: map[string]string{"memory": "ram"}, wantError: "state"},
		{name: "missing memory", artifacts: map[string]string{"state": "device"}, wantError: "memory"},
		{name: "unexpected config", artifacts: map[string]string{"state": "device", "memory": "ram", "config.json": "{}"}, wantError: "unexpected artifact"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := &fakeRuntimeCaptureSource{
				vmmName:           "stratovirt",
				memorySizeMB:      256,
				snapshotArtifacts: tt.artifacts,
			}
			_, err := NewFullCheckpointCapture().Capture(context.Background(), RuntimeCaptureRequest{
				Source:      source,
				PauseBefore: true,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Capture() error = %v, want %q", err, tt.wantError)
			}
			if got := strings.Join(source.events, ","); got != "pause,capture,resume" {
				t.Fatalf("events = %q", got)
			}
			stagingRoot := source.snapshotDir
			if _, statErr := os.Stat(stagingRoot); !os.IsNotExist(statErr) {
				t.Fatalf("failed staging root still exists: %v", statErr)
			}
		})
	}
}

func TestFullCheckpointCaptureRejectsInvalidMetadataBeforeLifecycleChange(t *testing.T) {
	for _, source := range []*fakeRuntimeCaptureSource{
		{vmmName: "stratovirt"},
		{vmmName: "unknown", memorySizeMB: 256},
	} {
		_, err := NewFullCheckpointCapture().Capture(context.Background(), RuntimeCaptureRequest{
			Source:      source,
			PauseBefore: true,
		})
		if err == nil {
			t.Fatal("Capture() error = nil")
		}
		if len(source.events) != 0 {
			t.Fatalf("invalid capture changed lifecycle: %#v", source.events)
		}
	}
}

func TestFullCheckpointCaptureErrorsRollbackLifecycleAndStaging(t *testing.T) {
	errPause := errors.New("pause failed")
	errCapture := errors.New("VMM capture failed")
	errResume := errors.New("resume failed")

	tests := []struct {
		name               string
		configure          func(*fakeRuntimeCaptureSource)
		pauseBefore        bool
		wantErrors         []error
		wantResumeSentinel bool
		wantEvents         string
		wantStatePath      bool
	}{
		{
			name: "pause failure does not resume a VM the adapter did not pause",
			configure: func(source *fakeRuntimeCaptureSource) {
				source.pauseErr = errPause
			},
			pauseBefore: true,
			wantErrors:  []error{errPause},
			wantEvents:  "pause",
		},
		{
			name: "capture and resume errors are joined",
			configure: func(source *fakeRuntimeCaptureSource) {
				source.captureErr = errCapture
				source.resumeErr = errResume
			},
			pauseBefore:        true,
			wantErrors:         []error{errCapture, errResume},
			wantResumeSentinel: true,
			wantEvents:         "pause,capture,resume",
			wantStatePath:      true,
		},
		{
			name: "memory copy failure resumes running source",
			configure: func(source *fakeRuntimeCaptureSource) {
				source.memoryPath = filepath.Join(source.memoryPath, "missing")
			},
			pauseBefore:   true,
			wantEvents:    "pause,capture,resume",
			wantStatePath: true,
		},
		{
			name: "resume failure discards otherwise complete capture",
			configure: func(source *fakeRuntimeCaptureSource) {
				source.resumeErr = errResume
			},
			pauseBefore:        true,
			wantErrors:         []error{errResume},
			wantResumeSentinel: true,
			wantEvents:         "pause,capture,resume",
			wantStatePath:      true,
		},
		{
			name: "suspended capture failure never resumes",
			configure: func(source *fakeRuntimeCaptureSource) {
				source.captureErr = errCapture
				source.resumeErr = errResume
			},
			pauseBefore:   false,
			wantErrors:    []error{errCapture},
			wantEvents:    "capture",
			wantStatePath: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempRoot := t.TempDir()
			memoryPath, _ := writeSparseMemoryBacking(t, tempRoot)
			source := &fakeRuntimeCaptureSource{
				memoryPath:   memoryPath,
				memorySizeMB: 512,
				vmmName:      "cloud-hypervisor",
			}
			tt.configure(source)

			captured, err := NewFullCheckpointCapture().Capture(context.Background(), RuntimeCaptureRequest{
				Source:      source,
				PauseBefore: tt.pauseBefore,
			})
			if err == nil {
				_ = os.RemoveAll(captured.MemRootPath)
				t.Fatal("Capture() error = nil")
			}
			for _, wantErr := range tt.wantErrors {
				if !errors.Is(err, wantErr) {
					t.Fatalf("Capture() error = %v, want errors.Is(%v)", err, wantErr)
				}
			}
			if got := errors.Is(err, ErrCheckpointResume); got != tt.wantResumeSentinel {
				t.Fatalf("errors.Is(ErrCheckpointResume) = %v, want %v; error: %v", got, tt.wantResumeSentinel, err)
			}
			if got := strings.Join(source.events, ","); got != tt.wantEvents {
				t.Fatalf("capture events = %q, want %q", got, tt.wantEvents)
			}
			if tt.wantStatePath {
				memRoot := filepath.Dir(source.snapshotDir)
				if _, statErr := os.Stat(memRoot); !os.IsNotExist(statErr) {
					t.Fatalf("failed capture staging still exists at %s: %v", memRoot, statErr)
				}
			} else {
				entries, readErr := os.ReadDir(tempRoot)
				if readErr != nil {
					t.Fatalf("read staging parent: %v", readErr)
				}
				// The source memory backing is the only file that should remain.
				if len(entries) != 1 || entries[0].Name() != filepath.Base(memoryPath) {
					t.Fatalf("staging cleanup left entries: %#v", entries)
				}
			}
		})
	}
}

func TestSandboxRuntimeCaptureMetadataComesFromLaunchSpec(t *testing.T) {
	source := &Sandbox{
		vmmName: "cloud-hypervisor",
		vmStartSpec: VMStartSpec{
			MemoryPath:   "/runtime/mem/mem.img",
			MemorySizeMB: 768,
		},
	}

	if got := source.MemoryBackingPath(); got != "/runtime/mem/mem.img" {
		t.Fatalf("MemoryBackingPath() = %q", got)
	}
	if got := source.VMMName(); got != "cloud-hypervisor" {
		t.Fatalf("VMMName() = %q", got)
	}
	if got := source.MemorySizeMB(); got != 768 {
		t.Fatalf("MemorySizeMB() = %d", got)
	}
}

type fakeRuntimeCaptureSource struct {
	events            []string
	memoryPath        string
	memorySizeMB      int64
	vmmName           string
	snapshotDir       string
	snapshotArtifacts map[string]string
	expectedMemory    []byte
	checkCopyOnResume bool
	resumeCheckErr    error
	pauseErr          error
	captureErr        error
	resumeErr         error
}

func (f *fakeRuntimeCaptureSource) Pause(context.Context) error {
	f.events = append(f.events, "pause")
	return f.pauseErr
}

func (f *fakeRuntimeCaptureSource) Resume(context.Context) error {
	f.events = append(f.events, "resume")
	if f.checkCopyOnResume {
		memRoot := filepath.Dir(f.snapshotDir)
		got, err := os.ReadFile(filepath.Join(memRoot, capturedMemoryFileName))
		if err != nil {
			f.resumeCheckErr = err
		} else if !reflect.DeepEqual(got, f.expectedMemory) {
			f.resumeCheckErr = fmt.Errorf("captured bytes differ")
		}
	}
	return f.resumeErr
}

func (f *fakeRuntimeCaptureSource) CreateVMMState(_ context.Context, snapshotDir string) error {
	f.events = append(f.events, "capture")
	f.snapshotDir = snapshotDir
	artifacts := f.snapshotArtifacts
	if artifacts == nil && f.vmmName == "stratovirt" {
		artifacts = map[string]string{"state": "vmm-state", "memory": "guest-memory"}
	} else if artifacts == nil {
		artifacts = map[string]string{"state.bin": "vmm-state"}
	}
	for name, data := range artifacts {
		if err := os.WriteFile(filepath.Join(snapshotDir, name), []byte(data), 0o640); err != nil {
			return err
		}
	}
	return f.captureErr
}

func (f *fakeRuntimeCaptureSource) MemoryBackingPath() string {
	return f.memoryPath
}

func (f *fakeRuntimeCaptureSource) MemorySizeMB() int64 {
	return f.memorySizeMB
}

func (f *fakeRuntimeCaptureSource) VMMName() string {
	return f.vmmName
}

func writeSparseMemoryBacking(t *testing.T, dir string) (string, []byte) {
	t.Helper()
	path := filepath.Join(dir, "runtime-mem.img")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o640)
	if err != nil {
		t.Fatalf("create memory backing: %v", err)
	}
	if _, err := file.WriteAt([]byte("memory-head"), 0); err != nil {
		_ = file.Close()
		t.Fatalf("write memory head: %v", err)
	}
	if _, err := file.WriteAt([]byte("memory-tail"), 128*1024); err != nil {
		_ = file.Close()
		t.Fatalf("write memory tail: %v", err)
	}
	if err := file.Truncate(256 * 1024); err != nil {
		_ = file.Close()
		t.Fatalf("truncate memory backing: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close memory backing: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod memory backing: %v", err)
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read memory backing: %v", err)
	}
	return path, expected
}
