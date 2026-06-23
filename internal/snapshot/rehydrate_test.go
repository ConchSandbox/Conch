package snapshot

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/daemon/state"
)

func TestRehydrateRuntimeStateSkipsAliasesForUnrestoredViews(t *testing.T) {
	ctx := context.Background()
	mountPoint := "/"
	unmountedMountPoint := t.TempDir()

	srv := &server{
		snt: fakeSnapshotter{
			mounts: []mount.Mount{{Type: "bind", Source: "/"}},
		},
		activeSnapshots:  make(map[string]map[string]*snapshots.Info),
		activeRootfsPmem: make(map[string]map[string][]string),
		viewMgr: &viewManager{
			viewMounts:  make(map[string]map[string]*viewMountRef),
			viewAliases: make(map[string]map[string]string),
		},
	}

	result, err := srv.rehydrateRuntimeState(ctx, nil, []state.ViewSnapshotRecord{
		{
			Namespace:        "default",
			ParentSnapshotID: "parent-ready",
			ViewSnapshotKey:  "view-ready",
			MountPoint:       mountPoint,
			RefCount:         1,
			State:            state.SandboxReady,
		},
		{
			Namespace:        "default",
			ParentSnapshotID: "parent-missing",
			ViewSnapshotKey:  "view-missing",
			MountPoint:       unmountedMountPoint,
			RefCount:         1,
			State:            state.SandboxReady,
		},
	}, []state.ViewAliasRecord{
		{
			Namespace:        "default",
			AliasKey:         "alias-ready",
			ParentSnapshotID: "parent-ready",
		},
		{
			Namespace:        "default",
			AliasKey:         "alias-missing",
			ParentSnapshotID: "parent-missing",
		},
	})
	if err == nil {
		t.Fatalf("RehydrateRuntimeState() error = nil, want missing mount error")
	}
	if result.ViewMounts != 1 {
		t.Fatalf("ViewMounts = %d, want 1", result.ViewMounts)
	}
	if result.ViewAliases != 1 {
		t.Fatalf("ViewAliases = %d, want 1", result.ViewAliases)
	}
	if parent, ok := srv.viewMgr.getViewAlias("default", "alias-ready"); !ok || parent != "parent-ready" {
		t.Fatalf("ready alias = (%q, %v), want parent-ready true", parent, ok)
	}
	if parent, ok := srv.viewMgr.getViewAlias("default", "alias-missing"); ok {
		t.Fatalf("missing alias restored to %q, want absent", parent)
	}
}

func TestGetOrCreateViewMountRecreatesStaleRestoredRef(t *testing.T) {
	ctx := context.Background()
	staleErr := errors.New("recreate view")
	mountPoint := t.TempDir()
	vm := &viewManager{
		viewMounts:  make(map[string]map[string]*viewMountRef),
		viewAliases: make(map[string]map[string]string),
	}
	vm.restoreViewMount("default", "parent", "view-parent", mountPoint, 1, []mount.Mount{{Type: "bind", Source: "/"}}, "")

	_, _, err := vm.getOrCreateViewMount(fakeSnapshotter{viewErr: staleErr}, nil, ctx, "default", "parent", "view-parent", mountPoint)
	if !errors.Is(err, staleErr) {
		t.Fatalf("getOrCreateViewMount() error = %v, want %v", err, staleErr)
	}
	if nsMap := vm.viewMounts["default"]; nsMap != nil {
		if _, ok := nsMap["parent"]; ok {
			t.Fatalf("stale view mount ref was not removed")
		}
	}
}

func TestGetOrCreateViewMountRechecksAfterStaleDeactivate(t *testing.T) {
	ctx := context.Background()
	mountPoint := t.TempDir()
	vm := &viewManager{
		viewMounts:  make(map[string]map[string]*viewMountRef),
		viewAliases: make(map[string]map[string]string),
	}
	vm.restoreViewMount("default", "parent", "view-parent", mountPoint, 1, []mount.Mount{{Type: "bind", Source: "/"}}, "stale-activation")

	deactivateStarted := make(chan struct{})
	allowDeactivate := make(chan struct{})
	mountMgr := &blockingMountManager{
		deactivateStarted: deactivateStarted,
		allowDeactivate:   allowDeactivate,
	}
	firstViewStarted := make(chan struct{}, 1)
	firstErrCh := make(chan error, 1)
	go func() {
		_, _, err := vm.getOrCreateViewMount(fakeSnapshotter{
			viewErr:     errors.New("first view should not run"),
			viewStarted: firstViewStarted,
		}, mountMgr, ctx, "default", "parent", "view-parent", mountPoint)
		firstErrCh <- err
	}()

	select {
	case <-deactivateStarted:
	case <-time.After(time.Second):
		t.Fatal("stale deactivate did not start")
	}

	secondViewStarted := make(chan struct{}, 1)
	unblockSecondView := make(chan struct{})
	secondErr := errors.New("second view failed")
	secondErrCh := make(chan error, 1)
	go func() {
		_, _, err := vm.getOrCreateViewMount(fakeSnapshotter{
			viewErr:     secondErr,
			viewStarted: secondViewStarted,
			unblockView: unblockSecondView,
		}, nil, ctx, "default", "parent", "view-parent", mountPoint)
		secondErrCh <- err
	}()

	select {
	case <-secondViewStarted:
	case <-time.After(time.Second):
		t.Fatal("second view did not start")
	}

	close(allowDeactivate)
	select {
	case <-firstViewStarted:
		t.Fatal("stale recovery overwrote the concurrent placeholder and started a second view")
	case <-time.After(50 * time.Millisecond):
	}

	close(unblockSecondView)
	if err := <-secondErrCh; !errors.Is(err, secondErr) {
		t.Fatalf("second getOrCreateViewMount() error = %v, want %v", err, secondErr)
	}
	if err := <-firstErrCh; err == nil {
		t.Fatal("first getOrCreateViewMount() error = nil, want placeholder failure")
	}
}

func TestReleaseViewMountIgnoresBareNotExistDeactivate(t *testing.T) {
	mountPoint := filepath.Join(t.TempDir(), "snapshot", "default", "missing-view")
	vm := &viewManager{
		viewMounts:  make(map[string]map[string]*viewMountRef),
		viewAliases: make(map[string]map[string]string),
	}
	vm.restoreViewMount("default", "parent", "view-parent", mountPoint, 1, nil, "view-parent")

	err := vm.releaseViewMount(fakeSnapshotter{}, errorMountManager{deactivateErr: unix.ENOENT}, "default", "parent")
	if err != nil {
		t.Fatalf("releaseViewMount() error = %v, want nil for idempotent missing mount cleanup", err)
	}
	if nsMap := vm.viewMounts["default"]; nsMap != nil {
		if _, ok := nsMap["parent"]; ok {
			t.Fatalf("view mount ref was not released")
		}
	}
}

type fakeSnapshotter struct {
	viewErr     error
	mounts      []mount.Mount
	viewStarted chan struct{}
	unblockView chan struct{}
}

func (fakeSnapshotter) Prepare(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (f fakeSnapshotter) View(context.Context, string, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	if f.viewStarted != nil {
		select {
		case f.viewStarted <- struct{}{}:
		default:
		}
	}
	if f.unblockView != nil {
		<-f.unblockView
	}
	if f.viewErr != nil {
		return nil, f.viewErr
	}
	return f.mounts, nil
}

func (f fakeSnapshotter) Mounts(context.Context, string, string) ([]mount.Mount, error) {
	return f.mounts, nil
}

func (fakeSnapshotter) Commit(context.Context, string, string, string, ...snapshots.Opt) error {
	return nil
}

func (fakeSnapshotter) Update(context.Context, string, snapshots.Info, ...string) (snapshots.Info, error) {
	return snapshots.Info{}, nil
}

func (fakeSnapshotter) Remove(context.Context, string, string) error {
	return nil
}

func (fakeSnapshotter) Stat(context.Context, string, string) (snapshots.Info, error) {
	return snapshots.Info{}, nil
}

func (fakeSnapshotter) List(context.Context, string, map[string]*snapshots.Info, ...string) error {
	return nil
}

func (fakeSnapshotter) ListNamespaces(context.Context) ([]string, error) {
	return nil, nil
}

func (fakeSnapshotter) Close() error {
	return nil
}

type blockingMountManager struct {
	deactivateStarted chan struct{}
	allowDeactivate   chan struct{}
}

func (m *blockingMountManager) Activate(context.Context, string, []mount.Mount, ...mount.ActivateOpt) (mount.ActivationInfo, error) {
	return mount.ActivationInfo{}, nil
}

func (m *blockingMountManager) Deactivate(context.Context, string) error {
	select {
	case m.deactivateStarted <- struct{}{}:
	default:
	}
	<-m.allowDeactivate
	return nil
}

func (m *blockingMountManager) Info(context.Context, string) (mount.ActivationInfo, error) {
	return mount.ActivationInfo{}, nil
}

func (m *blockingMountManager) Update(context.Context, mount.ActivationInfo, ...string) (mount.ActivationInfo, error) {
	return mount.ActivationInfo{}, nil
}

func (m *blockingMountManager) List(context.Context, ...string) ([]mount.ActivationInfo, error) {
	return nil, nil
}

type errorMountManager struct {
	deactivateErr error
}

func (m errorMountManager) Activate(context.Context, string, []mount.Mount, ...mount.ActivateOpt) (mount.ActivationInfo, error) {
	return mount.ActivationInfo{}, nil
}

func (m errorMountManager) Deactivate(context.Context, string) error {
	return m.deactivateErr
}

func (m errorMountManager) Info(context.Context, string) (mount.ActivationInfo, error) {
	return mount.ActivationInfo{}, nil
}

func (m errorMountManager) Update(context.Context, mount.ActivationInfo, ...string) (mount.ActivationInfo, error) {
	return mount.ActivationInfo{}, nil
}

func (m errorMountManager) List(context.Context, ...string) ([]mount.ActivationInfo, error) {
	return nil, nil
}
