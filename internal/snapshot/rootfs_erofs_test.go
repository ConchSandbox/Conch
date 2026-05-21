package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
)

func TestSnapshotConfigSnapDirTreatsRootDirAsMemRelative(t *testing.T) {
	conf := &SnapshotConfig{
		MemDir:  "/var/lib/conch/mem",
		RootDir: "/conch/snapshot",
	}
	if got, want := conf.SnapDir(), "/var/lib/conch/mem/conch/snapshot"; got != want {
		t.Fatalf("SnapDir() = %q, want %q", got, want)
	}
}

func TestStatReturnsActiveSnapshotBeforeSnapshotterLookup(t *testing.T) {
	origServer := gServer
	t.Cleanup(func() {
		gServer = origServer
	})

	gServer = server{
		snt:             statMissingSnapshotter{},
		activeSnapshots: make(map[string]map[string]*snapshots.Info),
	}
	gServer.addActiveSnapshot("default", "active-rootfs", &snapshots.Info{
		Name:   "active-rootfs",
		Parent: "parent-rootfs",
	})

	info, err := Stat(context.Background(), "default", "active-rootfs")
	if err != nil {
		t.Fatalf("Stat active snapshot: %v", err)
	}
	if info.Parent != "parent-rootfs" {
		t.Fatalf("Parent = %q, want parent-rootfs", info.Parent)
	}
}

func TestCleanupEmptySnapshotParentsKeepsNamespaceRoot(t *testing.T) {
	namespaceRoot := filepath.Join(t.TempDir(), "snapshot", "default")
	mountPoint := filepath.Join(namespaceRoot, "sandbox-id", "mem")
	if err := os.MkdirAll(mountPoint, 0o750); err != nil {
		t.Fatalf("MkdirAll mount point: %v", err)
	}
	if err := os.Remove(mountPoint); err != nil {
		t.Fatalf("remove mount point: %v", err)
	}
	if err := cleanupEmptySnapshotParents(mountPoint); err != nil {
		t.Fatalf("cleanupEmptySnapshotParents: %v", err)
	}
	if _, err := os.Stat(namespaceRoot); err != nil {
		t.Fatalf("namespace root should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(mountPoint)); !os.IsNotExist(err) {
		t.Fatalf("sandbox dir should be pruned, err=%v", err)
	}
}

func TestPmemFilesFromErofsMountsUsesSourceAndDeviceOptions(t *testing.T) {
	got, err := pmemFilesFromErofsMounts([]mount.Mount{
		{Type: "overlay", Source: "overlay"},
		{Type: "erofs", Source: "/var/lib/containerd/erofs/layer0.erofs", Options: []string{
			"ro",
			"device=/var/lib/containerd/erofs/layer1.erofs",
			"device=/var/lib/containerd/erofs/layer1.erofs",
		}},
		{Type: "erofs", Source: "/var/lib/containerd/erofs/layer2.erofs"},
	})
	if err != nil {
		t.Fatalf("pmemFilesFromErofsMounts: %v", err)
	}
	want := []string{
		"/var/lib/containerd/erofs/layer0.erofs",
		"/var/lib/containerd/erofs/layer1.erofs",
		"/var/lib/containerd/erofs/layer2.erofs",
	}
	if len(got) != len(want) {
		t.Fatalf("files = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("files = %#v, want %#v", got, want)
		}
	}
}

func TestPmemFilesFromErofsMountsRejectsMissingPaths(t *testing.T) {
	_, err := pmemFilesFromErofsMounts([]mount.Mount{{Type: "overlay", Source: "overlay"}})
	if err == nil {
		t.Fatal("expected missing erofs pmem files to fail")
	}
	if !strings.Contains(err.Error(), "contain no pmem files") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPmemFilesFromErofsMountsRejectsRelativePath(t *testing.T) {
	_, err := pmemFilesFromErofsMounts([]mount.Mount{{Type: "erofs", Source: "relative.erofs"}})
	if err == nil {
		t.Fatal("expected relative path to fail")
	}
	if !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSelectSnapshotRestorePmemFilesKeepsOriginalDeviceCount(t *testing.T) {
	files := []string{"/layer/new.erofs", "/layer/base0.erofs", "/layer/base1.erofs"}
	got, err := selectSnapshotRestorePmemFiles(files, 2)
	if err != nil {
		t.Fatalf("selectSnapshotRestorePmemFiles: %v", err)
	}
	want := []string{"/layer/base0.erofs", "/layer/base1.erofs"}
	if len(got) != len(want) {
		t.Fatalf("files = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("files = %#v, want %#v", got, want)
		}
	}
}

func TestSelectSnapshotRestorePmemFilesRejectsShortRootfsChain(t *testing.T) {
	_, err := selectSnapshotRestorePmemFiles([]string{"/layer/base0.erofs"}, 2)
	if err == nil {
		t.Fatal("expected short rootfs chain to fail")
	}
	if !strings.Contains(err.Error(), "less than snapshot device count") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type statMissingSnapshotter struct{}

func (statMissingSnapshotter) Prepare(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, fmt.Errorf("unexpected Prepare")
}

func (statMissingSnapshotter) View(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, fmt.Errorf("unexpected View")
}

func (statMissingSnapshotter) Mounts(context.Context, string, string) ([]mount.Mount, error) {
	return nil, fmt.Errorf("unexpected Mounts")
}

func (statMissingSnapshotter) Commit(context.Context, string, string, string, ...snapshots.Opt) error {
	return fmt.Errorf("unexpected Commit")
}

func (statMissingSnapshotter) Update(context.Context, string, snapshots.Info, ...string) (snapshots.Info, error) {
	return snapshots.Info{}, fmt.Errorf("unexpected Update")
}

func (statMissingSnapshotter) Remove(context.Context, string, string) error {
	return fmt.Errorf("unexpected Remove")
}

func (statMissingSnapshotter) Stat(context.Context, string, string) (snapshots.Info, error) {
	return snapshots.Info{}, fmt.Errorf("snapshot does not exist")
}

func (statMissingSnapshotter) List(context.Context, string, map[string]*snapshots.Info, ...string) error {
	return fmt.Errorf("unexpected List")
}

func (statMissingSnapshotter) ListNamespaces(context.Context) ([]string, error) {
	return nil, fmt.Errorf("unexpected ListNamespaces")
}

func (statMissingSnapshotter) Close() error {
	return nil
}
