package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

func TestLoadCommittedBootLayoutMetadataUsesRootfsLabels(t *testing.T) {
	snapshotter := &recordingServerSnapshotter{
		statInfo: snapshots.Info{Labels: map[string]string{
			common.SnapshotLabelMemSize:     "1024",
			common.SnapshotLabelSnapshotDir: "custom/snapshot",
		}},
	}
	srv := &Server{snt: snapshotter}
	layout := &BootLayout{}
	layout.initDefaults()

	memorySizeFromSnapshot, err := srv.loadCommittedBootLayoutMetadata(context.Background(), "default", ParentSnapshotIDs{Rootfs: "rootfs-id"}, layout)
	if err != nil {
		t.Fatalf("loadCommittedBootLayoutMetadata() error = %v", err)
	}
	if !memorySizeFromSnapshot {
		t.Fatal("memorySizeFromSnapshot = false, want true")
	}
	if layout.MemorySizeMB != 1024 {
		t.Fatalf("MemorySizeMB = %d, want 1024", layout.MemorySizeMB)
	}
	if layout.SnapshotDir != "custom/snapshot" {
		t.Fatalf("SnapshotDir = %q, want custom/snapshot", layout.SnapshotDir)
	}
}

func TestPrepareRootfsSnapshotUpdatesLayoutAndActivePmem(t *testing.T) {
	snapshotter := &recordingServerSnapshotter{
		prepareMounts: []mount.Mount{
			{Type: "erofs", Source: "/layers/rootfs.erofs"},
			{Type: "erofs", Source: "/layers/base.erofs"},
		},
		statInfo: snapshots.Info{Name: "sandbox-rootfs", Parent: "parent-rootfs"},
	}
	srv := &Server{snt: snapshotter}
	layout := &BootLayout{}
	layout.initDefaults()

	if err := srv.prepareRootfsSnapshot(context.Background(), "default", "sandbox-rootfs", "parent-rootfs", layout); err != nil {
		t.Fatalf("prepareRootfsSnapshot() error = %v", err)
	}

	want := []string{"/layers/rootfs.erofs", "/layers/base.erofs"}
	if !slices.Equal(layout.pmemFiles, want) {
		t.Fatalf("layout pmemFiles = %#v, want %#v", layout.pmemFiles, want)
	}
	activePmem := srv.getActiveRootfsPmem("default", "sandbox-rootfs")
	if !slices.Equal(activePmem, want) {
		t.Fatalf("active rootfs pmem = %#v, want %#v", activePmem, want)
	}
	activeSnapshot := srv.getActiveSnapshot("default", "sandbox-rootfs")
	if activeSnapshot == nil {
		t.Fatal("active snapshot was not registered")
	}
	if activeSnapshot.Parent != "parent-rootfs" {
		t.Fatalf("active snapshot parent = %q, want parent-rootfs", activeSnapshot.Parent)
	}
}

func TestPrepareAndRegisterSnapshotRollsBackPreparedSnapshotOnMountError(t *testing.T) {
	snapshotter := &recordingServerSnapshotter{}
	srv := &Server{snt: snapshotter}
	mountPoint := filepath.Join(t.TempDir(), "mem")

	if _, err := srv.prepareAndMountActiveSnapshot(context.Background(), "default", "mem-active", "parent-mem", mountPoint); err == nil {
		t.Fatal("prepareAndMountActiveSnapshot() error = nil, want mount error")
	}
	if !slices.Contains(snapshotter.removedKeys, "mem-active") {
		t.Fatalf("removed snapshot keys = %#v, want mem-active", snapshotter.removedKeys)
	}
	if _, err := os.Stat(mountPoint); !os.IsNotExist(err) {
		t.Fatalf("mount point still exists or stat failed with unexpected error: %v", err)
	}
	if active := srv.getActiveSnapshot("default", "mem-active"); active != nil {
		t.Fatalf("active snapshot registered after rollback: %#v", active)
	}
}

func TestReleaseBootLayoutDoesNotRemoveCommittedSnapshotWithSandboxKey(t *testing.T) {
	snapshotter := &recordingServerSnapshotter{
		statInfo: snapshots.Info{Kind: snapshots.KindCommitted},
	}
	srv := &Server{snt: snapshotter, workDir: t.TempDir()}

	if err := srv.ReleaseBootLayout(context.Background(), "sandbox-1"); err != nil {
		t.Fatalf("ReleaseBootLayout() error = %v", err)
	}
	if len(snapshotter.removedKeys) != 0 {
		t.Fatalf("removed keys = %#v, want none", snapshotter.removedKeys)
	}
}

func TestBootLayoutMemoryPathsRespectStorageMode(t *testing.T) {
	for _, tt := range []struct {
		name           string
		mode           MemoryLayoutMode
		wantMemoryPath bool
		wantSnapPath   bool
	}{
		{name: "none", mode: MemoryLayoutNone},
		{name: "writable file", mode: MemoryLayoutWritableFile, wantMemoryPath: true, wantSnapPath: true},
		{name: "checkpoint view", mode: MemoryLayoutCheckpointView, wantSnapPath: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			layout := &BootLayout{
				MemMount:     "/runtime/mem",
				SnapshotDir:  "conch/snapshot",
				MemoryLayout: tt.mode,
			}
			if got := layout.SnapshotMemFile() != ""; got != tt.wantMemoryPath {
				t.Fatalf("SnapshotMemFile() = %q", layout.SnapshotMemFile())
			}
			if got := layout.SnapDir() != ""; got != tt.wantSnapPath {
				t.Fatalf("SnapDir() = %q", layout.SnapDir())
			}
		})
	}
}

func TestReleaseBootLayoutReleasesCheckpointMemoryView(t *testing.T) {
	const key = "sandbox-view"
	snapshotter := &recordingServerSnapshotter{statByKey: map[string]snapshots.Info{
		getRootfsViewSnapshotKey(key): {Kind: snapshots.KindView},
		getMemViewSnapshotKey(key):    {Kind: snapshots.KindView},
		getVMViewSnapshotKey(key):     {Kind: snapshots.KindView},
	}}
	srv := &Server{snt: snapshotter, workDir: t.TempDir()}

	if err := srv.ReleaseBootLayout(context.Background(), key); err != nil {
		t.Fatalf("ReleaseBootLayout() error = %v", err)
	}
	want := []string{getRootfsViewSnapshotKey(key), getMemViewSnapshotKey(key), getVMViewSnapshotKey(key)}
	for _, viewKey := range want {
		if !slices.Contains(snapshotter.removedKeys, viewKey) {
			t.Fatalf("removed keys = %#v, missing %s", snapshotter.removedKeys, viewKey)
		}
	}
}

type recordingServerSnapshotter struct {
	prepareMounts []mount.Mount
	prepareErr    error
	statInfo      snapshots.Info
	statByKey     map[string]snapshots.Info
	statErr       error
	removedKeys   []string
}

func (r *recordingServerSnapshotter) Prepare(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	if r.prepareErr != nil {
		return nil, r.prepareErr
	}
	return append([]mount.Mount(nil), r.prepareMounts...), nil
}

func (r *recordingServerSnapshotter) View(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (r *recordingServerSnapshotter) Mounts(context.Context, string) ([]mount.Mount, error) {
	return nil, nil
}

func (r *recordingServerSnapshotter) Commit(context.Context, string, string, ...snapshots.Opt) error {
	return nil
}

func (r *recordingServerSnapshotter) Update(context.Context, snapshots.Info, ...string) (snapshots.Info, error) {
	return snapshots.Info{}, nil
}

func (r *recordingServerSnapshotter) Remove(_ context.Context, key string) error {
	r.removedKeys = append(r.removedKeys, key)
	return nil
}

func (r *recordingServerSnapshotter) Stat(_ context.Context, key string) (snapshots.Info, error) {
	if r.statErr != nil {
		return snapshots.Info{}, r.statErr
	}
	if r.statByKey != nil {
		info, ok := r.statByKey[key]
		if !ok {
			return snapshots.Info{}, errors.New("snapshot not found")
		}
		return info, nil
	}
	return r.statInfo, nil
}

func (r *recordingServerSnapshotter) List(context.Context, map[string]*snapshots.Info, ...string) error {
	return nil
}

func (r *recordingServerSnapshotter) Close() error {
	return nil
}
