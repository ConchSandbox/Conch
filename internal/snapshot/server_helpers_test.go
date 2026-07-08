package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

func TestCommitMemSnapshotLabelsComponentKind(t *testing.T) {
	snapshotter := &recordingServerSnapshotter{}
	srv := &Server{snt: snapshotter}

	if err := srv.commitMemSnapshot(context.Background(), "default", "mem-active", "mem-committed", "rootfs-id"); err != nil {
		t.Fatalf("commitMemSnapshot() error = %v", err)
	}
	if snapshotter.committedInfo.Labels[common.SnapshotLabelGroupID] != "rootfs-id" {
		t.Fatalf("group id label = %q, want rootfs-id", snapshotter.committedInfo.Labels[common.SnapshotLabelGroupID])
	}
	if snapshotter.committedInfo.Labels[common.SnapshotLabelComponentKind] != common.SnapshotComponentKindMem {
		t.Fatalf("component kind = %q, want %q", snapshotter.committedInfo.Labels[common.SnapshotLabelComponentKind], common.SnapshotComponentKindMem)
	}
}

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

type recordingServerSnapshotter struct {
	committedInfo snapshots.Info
	prepareMounts []mount.Mount
	prepareErr    error
	statInfo      snapshots.Info
	statErr       error
	removedKeys   []string
}

func (r *recordingServerSnapshotter) Prepare(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	if r.prepareErr != nil {
		return nil, r.prepareErr
	}
	return append([]mount.Mount(nil), r.prepareMounts...), nil
}

func (r *recordingServerSnapshotter) View(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (r *recordingServerSnapshotter) Mounts(context.Context, string, string) ([]mount.Mount, error) {
	return nil, nil
}

func (r *recordingServerSnapshotter) Commit(_ context.Context, _, _, snapshotID string, opts ...snapshots.Opt) error {
	info := snapshots.Info{Name: snapshotID}
	for _, opt := range opts {
		if err := opt(&info); err != nil {
			return err
		}
	}
	r.committedInfo = info
	return nil
}

func (r *recordingServerSnapshotter) Update(context.Context, string, snapshots.Info, ...string) (snapshots.Info, error) {
	return snapshots.Info{}, nil
}

func (r *recordingServerSnapshotter) Remove(_ context.Context, _, key string) error {
	r.removedKeys = append(r.removedKeys, key)
	return nil
}

func (r *recordingServerSnapshotter) Stat(context.Context, string, string) (snapshots.Info, error) {
	if r.statErr != nil {
		return snapshots.Info{}, r.statErr
	}
	return r.statInfo, nil
}

func (r *recordingServerSnapshotter) List(context.Context, string, map[string]*snapshots.Info, ...string) error {
	return nil
}

func (r *recordingServerSnapshotter) ListNamespaces(context.Context) ([]string, error) {
	return nil, nil
}

func (r *recordingServerSnapshotter) Close() error {
	return nil
}
