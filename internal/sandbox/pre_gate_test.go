package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func evictPreGateTestFile(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurePreGateRecordsFirstRestoreWithoutGate(t *testing.T) {
	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshot")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	memoryPath := filepath.Join(snapshotDir, "memory")
	if err := os.WriteFile(memoryPath, make([]byte, os.Getpagesize()), 0o600); err != nil {
		t.Fatal(err)
	}
	evictPreGateTestFile(t, memoryPath)
	spec := VMStartSpec{SnapfilePath: snapshotDir, PreGateKey: "sha256:first", PreGateRequired: true}
	if err := configurePreGate(context.Background(), &spec, "sandbox-a", stateDir); err != nil {
		t.Fatal(err)
	}
	if spec.RecordPreGatePath == "" || spec.ResumeGatePath != "" {
		t.Fatalf("record path = %q, gate path = %q", spec.RecordPreGatePath, spec.ResumeGatePath)
	}
	if filepath.Dir(spec.RecordPreGatePath) != stateDir {
		t.Fatalf("record path %q is outside state dir", spec.RecordPreGatePath)
	}
}

func TestConfigurePreGateFirstRestoreHonorsCancelledMaterialization(t *testing.T) {
	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshot")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	memoryPath := filepath.Join(snapshotDir, "memory")
	if err := os.WriteFile(memoryPath, make([]byte, os.Getpagesize()), 0o600); err != nil {
		t.Fatal(err)
	}
	evictPreGateTestFile(t, memoryPath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spec := VMStartSpec{SnapfilePath: snapshotDir, PreGateKey: "sha256:cancelled", PreGateRequired: true}
	if err := configurePreGate(ctx, &spec, "sandbox-cancelled", stateDir); err == nil {
		t.Fatal("configurePreGate() succeeded after materialization was cancelled")
	}
	if spec.RecordPreGatePath != "" || spec.ResumeGatePath != "" {
		t.Fatalf("cancelled materialization configured pre-gate: record=%q gate=%q", spec.RecordPreGatePath, spec.ResumeGatePath)
	}
}

func TestConfigurePreGateWarmsKnownProfileAndSignalsGate(t *testing.T) {
	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshot")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	memory := make([]byte, 3*os.Getpagesize())
	memoryPath := filepath.Join(snapshotDir, "memory")
	if err := os.WriteFile(memoryPath, memory, 0o600); err != nil {
		t.Fatal(err)
	}
	evictPreGateTestFile(t, memoryPath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := "sha256:known"
	profile := preGateProfile{
		Version:  preGateProfileVersion,
		PageSize: int64(os.Getpagesize()),
		FileSize: int64(len(memory)),
		Offsets:  []uint64{uint64(os.Getpagesize())},
	}
	data, _ := json.Marshal(profile)
	if err := os.WriteFile(filepath.Join(stateDir, profileFileName(key)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := VMStartSpec{SnapfilePath: snapshotDir, PreGateKey: key, PreGateRequired: true}
	if err := configurePreGate(context.Background(), &spec, "sandbox-b", stateDir); err != nil {
		t.Fatal(err)
	}
	defer cleanupResumeGate(spec.ResumeGatePath)
	if spec.ResumeGatePath == "" || spec.RecordPreGatePath != "" {
		t.Fatalf("record path = %q, gate path = %q", spec.RecordPreGatePath, spec.ResumeGatePath)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate, err := os.ReadFile(spec.ResumeGatePath)
		if err == nil && len(gate) == resumeGateSize && binary.LittleEndian.Uint32(gate) == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("resume gate was not signaled after full memory warmup")
}

func TestConfigurePreGateKeepsGateClosedWhenFullMaterializationFails(t *testing.T) {
	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshot")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	memory := make([]byte, 3*os.Getpagesize())
	memoryPath := filepath.Join(snapshotDir, "memory")
	if err := os.WriteFile(memoryPath, memory, 0o600); err != nil {
		t.Fatal(err)
	}
	evictPreGateTestFile(t, memoryPath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := "sha256:failed-full-read"
	profile := preGateProfile{
		Version:  preGateProfileVersion,
		PageSize: int64(os.Getpagesize()),
		FileSize: int64(len(memory)),
		Offsets:  []uint64{uint64(os.Getpagesize())},
	}
	data, _ := json.Marshal(profile)
	if err := os.WriteFile(filepath.Join(stateDir, profileFileName(key)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spec := VMStartSpec{SnapfilePath: snapshotDir, PreGateKey: key, PreGateRequired: true}
	if err := configurePreGate(ctx, &spec, "sandbox-failed-full-read", stateDir); err != nil {
		t.Fatal(err)
	}
	defer cleanupResumeGate(spec.ResumeGatePath)
	time.Sleep(20 * time.Millisecond)
	gate, err := os.ReadFile(spec.ResumeGatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(gate); got != 0 {
		t.Fatalf("gate value = %d after failed materialization, want 0", got)
	}
}

func TestConfigurePreGateSkipsFullyResidentMemory(t *testing.T) {
	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshot")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	memoryPath := filepath.Join(snapshotDir, "memory")
	if err := os.WriteFile(memoryPath, make([]byte, 2*os.Getpagesize()), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(memoryPath)
	if err != nil || len(data) == 0 {
		t.Fatalf("read resident memory: len=%d err=%v", len(data), err)
	}

	spec := VMStartSpec{SnapfilePath: snapshotDir, PreGateKey: "sha256:local"}
	if err := configurePreGate(context.Background(), &spec, "sandbox-local", stateDir); err != nil {
		t.Fatal(err)
	}
	if spec.RecordPreGatePath != "" || spec.ResumeGatePath != "" {
		t.Fatalf("local snapshot configured pre-gate: record=%q gate=%q", spec.RecordPreGatePath, spec.ResumeGatePath)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("local snapshot created pre-gate state: %v", err)
	}
}

func TestConfigurePreGateSkipsCompleteLocalSnapshot(t *testing.T) {
	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshot")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	memoryPath := filepath.Join(snapshotDir, "memory")
	if err := os.WriteFile(memoryPath, make([]byte, os.Getpagesize()), 0o600); err != nil {
		t.Fatal(err)
	}
	evictPreGateTestFile(t, memoryPath)

	spec := VMStartSpec{SnapfilePath: snapshotDir, PreGateKey: "sha256:local-cold"}
	if err := configurePreGate(context.Background(), &spec, "sandbox-local-cold", stateDir); err != nil {
		t.Fatal(err)
	}
	if spec.RecordPreGatePath != "" || spec.ResumeGatePath != "" {
		t.Fatalf("local snapshot configured pre-gate: record=%q gate=%q", spec.RecordPreGatePath, spec.ResumeGatePath)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("local snapshot created pre-gate state: %v", err)
	}
}

func TestLoadPreGateProfileRejectsDifferentMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	data, _ := json.Marshal(preGateProfile{
		Version: preGateProfileVersion, PageSize: int64(os.Getpagesize()), FileSize: 4096,
	})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPreGateProfile(path, 8192); err == nil {
		t.Fatal("profile for a different memory file was accepted")
	}
}
