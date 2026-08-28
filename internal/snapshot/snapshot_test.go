package snapshot_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/adapters/containerd/host"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
)

func TestCreateBootLayout(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("erofs snapshot integration test requires root privileges")
	}

	host, err := containerdhost.Start(context.Background(), containerdhost.Config{
		RootDir:  t.TempDir(),
		StateDir: t.TempDir(),
		Snapshot: containerdhost.SnapshotConfig{
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		t.Fatalf("start embedded containerd: %v", err)
	}
	defer host.Close()

	workDir := t.TempDir()
	server, err := snapshot.NewServer(workDir, host.Client())
	if err != nil {
		t.Fatalf("init server with %s: %v", workDir, err)
	}
	defer server.Close()

	ns := containerdclient.Namespace
	rootfsParent := "test-rootfs-parent"
	memParent := "test-mem-parent"
	vmParent := "test-vm-parent"
	parentCtx, err := host.Client().WithNamespace(context.Background())
	if err != nil {
		t.Fatalf("create namespace session: %v", err)
	}
	parentCtx, done, err := host.Client().WithLease(parentCtx)
	if err != nil {
		t.Fatalf("create parent snapshot lease: %v", err)
	}
	defer done(parentCtx)
	if err := createCommittedParent(parentCtx, host.Client().SnapshotService("erofs"), ns, rootfsParent, populateRootfsParent); err != nil {
		if isMountPermissionError(err) {
			t.Skipf("erofs snapshot integration test requires mount privileges: %v", err)
		}
		t.Fatalf("create rootfs parent snapshot: %v", err)
	}
	if err := createCommittedParent(parentCtx, host.Client().SnapshotService("erofs"), ns, memParent, populateMemParent); err != nil {
		if isMountPermissionError(err) {
			t.Skipf("erofs snapshot integration test requires mount privileges: %v", err)
		}
		t.Fatalf("create mem parent snapshot: %v", err)
	}
	if err := createCommittedParent(parentCtx, host.Client().SnapshotService("erofs"), ns, vmParent, populateVMParent); err != nil {
		if isMountPermissionError(err) {
			t.Skipf("erofs snapshot integration test requires mount privileges: %v", err)
		}
		t.Fatalf("create vm parent snapshot: %v", err)
	}

	key := "hello"
	parents := snapshot.ParentSnapshotIDs{
		Rootfs: rootfsParent,
		Mem:    memParent,
		VM:     vmParent,
	}
	runtimeCtx := containerdclient.NewNamespaceContext(context.Background())
	layout, err := server.CreateBootLayout(runtimeCtx, key, snapshot.BootLayoutRequest{
		Parents:      parents,
		MemoryLayout: snapshot.MemoryLayoutWritableFile,
	})
	if err != nil {
		if isMountPermissionError(err) {
			t.Skipf("erofs snapshot integration test requires mount privileges: %v", err)
		}
		t.Fatalf("get error: %v\n", err)
	}
	wantRefs := []snapshot.RuntimeSnapshotRef{
		{Snapshotter: "erofs", Role: "rootfs", Key: key},
		{Snapshotter: "erofs", Role: "vm", Key: "view-vm-" + key},
		{Snapshotter: "erofs", Role: "memory", Key: key + "-mem"},
	}
	if !reflect.DeepEqual(layout.RuntimeSnapshots, wantRefs) {
		t.Fatalf("runtime snapshot refs = %#v, want %#v", layout.RuntimeSnapshots, wantRefs)
	}
	activePrepared := true
	defer func() {
		if activePrepared {
			_ = server.ReleaseBootLayout(runtimeCtx, key)
		}
	}()
	t.Logf("create layout result: %v\n", layout)

	t.Logf("run release active layout: %s\n", key)
	if err := server.ReleaseBootLayout(runtimeCtx, key); err != nil {
		t.Fatalf("release layout failed: %v\n", err)
	}
	activePrepared = false
}

func createCommittedParent(ctx context.Context, snapshotter snapshots.Snapshotter, namespace, parent string, populate func(string) error) error {
	ctx = namespaces.WithNamespace(ctx, namespace)
	active := parent + "-active"
	mounts, err := snapshotter.Prepare(ctx, active, "")
	if err != nil {
		return err
	}
	defer snapshotter.Remove(ctx, active)

	mountPoint, err := os.MkdirTemp("", "conch-snapshot-parent-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountPoint)

	if len(mounts) > 0 {
		if err := mounts[0].Mount(mountPoint); err != nil {
			return err
		}
		defer mount.Unmount(mountPoint, 0)
		if populate != nil {
			if err := populate(mountPoint); err != nil {
				return err
			}
		}
	}
	return snapshotter.Commit(ctx, parent, active)
}

func populateRootfsParent(mountPoint string) error {
	return os.WriteFile(mountPoint+"/layer0.erofs", []byte("test"), 0o644)
}

func populateMemParent(mountPoint string) error {
	snapshotDir := mountPoint + "/conch/snapshot"
	if err := os.MkdirAll(snapshotDir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(mountPoint+"/"+common.MemFileName, []byte("mem"), 0o640); err != nil {
		return err
	}
	return os.WriteFile(snapshotDir+"/state", []byte("vmm-state"), 0o640)
}

func populateVMParent(mountPoint string) error {
	if err := os.MkdirAll(mountPoint+"/boot", 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(mountPoint+"/data", 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(mountPoint+"/"+common.VmKernelRelativePath, []byte("kernel"), 0o640); err != nil {
		return err
	}
	return os.WriteFile(mountPoint+"/"+common.VmInitrdRelativePath, []byte("initrd"), 0o640)
}

func isMountPermissionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return errors.Is(err, os.ErrPermission) ||
		strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "permission denied")
}
