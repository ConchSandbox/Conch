package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/snapshot/snapshotter"
)

// viewMountRef represents a shared view mount with reference counting.
type viewMountRef struct {
	namespace   string
	snapshotID  string
	snapshotKey string
	mountPoint  string
	refCount    int
	ready       chan struct{} // closed when mount is done
	initErr     error         // non-nil if mount failed
}

// viewManager manages shared view mounts for snapshot-based startup.
type viewManager struct {
	viewMounts  map[string]map[string]*viewMountRef // namespace -> parentSnapshotID -> viewMountRef
	viewAliases map[string]map[string]string        // namespace -> key -> parentSnapshotID
	viewLock    sync.Mutex
}

// addViewAliasWithoutLock adds a view alias (must be called with viewLock held).
func (vm *viewManager) addViewAliasWithoutLock(namespace, key, parentSnapshotID string) {
	if _, ok := vm.viewAliases[namespace]; !ok {
		vm.viewAliases[namespace] = make(map[string]string)
	}
	vm.viewAliases[namespace][key] = parentSnapshotID
}

// getViewAlias retrieves the parent snapshot ID for a given key.
func (vm *viewManager) getViewAlias(namespace, key string) (string, bool) {
	vm.viewLock.Lock()
	defer vm.viewLock.Unlock()
	if nsMap, ok := vm.viewAliases[namespace]; ok {
		if parent, ok := nsMap[key]; ok {
			return parent, true
		}
	}
	return "", false
}

// removeViewAlias removes a view alias for a specific key.
func (vm *viewManager) removeViewAlias(namespace, key string) {
	vm.viewLock.Lock()
	defer vm.viewLock.Unlock()

	if nsMap, ok := vm.viewAliases[namespace]; ok {
		delete(nsMap, key)
		if len(nsMap) == 0 {
			delete(vm.viewAliases, namespace)
		}
	}
}

// releaseViewAliases removes view aliases and releases their associated mounts.
// Returns (count of released aliases, error) where error contains any cleanup failures.
func (vm *viewManager) releaseViewAliases(snt snapshotter.Snapshotter, namespace string, keys ...string) (int, error) {
	var parents []string
	vm.viewLock.Lock()
	for _, key := range keys {
		if nsMap, ok := vm.viewAliases[namespace]; ok {
			if parent, ok := nsMap[key]; ok {
				delete(nsMap, key)
				parents = append(parents, parent)
			}
		}
	}
	if nsMap, ok := vm.viewAliases[namespace]; ok && len(nsMap) == 0 {
		delete(vm.viewAliases, namespace)
	}
	vm.viewLock.Unlock()

	var errs []error
	for _, parent := range parents {
		if err := vm.releaseViewMount(snt, namespace, parent); err != nil {
			errs = append(errs, err)
		}
	}
	return len(parents), errors.Join(errs...)
}

// acquireViewMount acquires or creates a shared view mount.
// Returns (cleaner, shared bool, error). Shared is true if reusing an existing mount.
func (vm *viewManager) acquireViewMount(
	snt snapshotter.Snapshotter,
	ctx context.Context,
	namespace, parentSnapshotID, viewAliasKey, viewSnapshotKey, mountPoint string,
	opts ...snapshots.Opt,
) (_ *snapshotCleaner, shared bool, err error) {
	ref, created, err := vm.getOrCreateViewMount(snt, ctx, namespace, parentSnapshotID, viewSnapshotKey, mountPoint, opts...)
	if err != nil {
		return nil, false, err
	}

	vm.viewLock.Lock()
	ref.refCount++
	vm.addViewAliasWithoutLock(namespace, viewAliasKey, parentSnapshotID)
	vm.viewLock.Unlock()

	return &snapshotCleaner{
		ctx:              ctx,
		viewMgr:          vm,
		snt:              snt,
		namespace:        namespace,
		key:              viewAliasKey,
		mountPoint:       mountPoint,
		viewed:           true,
		parentSnapshotID: parentSnapshotID,
	}, !created, nil
}

// ensureViewMount pre-creates a shared view mount with refCount=0.
func (vm *viewManager) ensureViewMount(
	snt snapshotter.Snapshotter,
	ctx context.Context,
	namespace, parentSnapshotID, viewSnapshotKey, mountPoint string,
	opts ...snapshots.Opt,
) error {
	_, _, err := vm.getOrCreateViewMount(snt, ctx, namespace, parentSnapshotID, viewSnapshotKey, mountPoint, opts...)
	return err
}

// getOrCreateViewMount returns an existing or newly created shared view mount.
// Newly created mounts start with refCount=0 and are promoted when a sandbox alias acquires them.
func (vm *viewManager) getOrCreateViewMount(
	snt snapshotter.Snapshotter,
	ctx context.Context,
	namespace, parentSnapshotID, viewSnapshotKey, mountPoint string,
	opts ...snapshots.Opt,
) (_ *viewMountRef, created bool, err error) {
	vm.viewLock.Lock()
	if nsMap, ok := vm.viewMounts[namespace]; ok {
		if ref, ok := nsMap[parentSnapshotID]; ok {
			if ref.ready != nil {
				readyCh := ref.ready
				vm.viewLock.Unlock()
				select {
				case <-readyCh:
				case <-ctx.Done():
					return nil, false, ctx.Err()
				}
				vm.viewLock.Lock()
				ref, ok = nsMap[parentSnapshotID]
				if !ok || ref.initErr != nil {
					vm.viewLock.Unlock()
					if !ok {
						return nil, false, fmt.Errorf("view mount for %s/%s was removed during init", namespace, parentSnapshotID)
					}
					return nil, false, fmt.Errorf("view mount for %s/%s init failed: %v", namespace, parentSnapshotID, ref.initErr)
				}
			}
			if ref.mountPoint != mountPoint {
				vm.viewLock.Unlock()
				return nil, false, fmt.Errorf("view mount path mismatch for %s/%s: %s vs %s", namespace, parentSnapshotID, ref.mountPoint, mountPoint)
			}
			vm.viewLock.Unlock()
			return ref, false, nil
		}
	}

	readyCh := make(chan struct{})
	placeholder := &viewMountRef{
		namespace:   namespace,
		snapshotID:  parentSnapshotID,
		snapshotKey: viewSnapshotKey,
		mountPoint:  mountPoint,
		refCount:    0,
		ready:       readyCh,
	}
	if _, ok := vm.viewMounts[namespace]; !ok {
		vm.viewMounts[namespace] = make(map[string]*viewMountRef)
	}
	vm.viewMounts[namespace][parentSnapshotID] = placeholder
	vm.viewLock.Unlock()

	mounts, err := snt.View(ctx, namespace, viewSnapshotKey, parentSnapshotID, opts...)
	if err != nil {
		vm.removePlaceholder(namespace, parentSnapshotID, readyCh, err)
		return nil, false, err
	}
	if len(mounts) != 1 {
		mountErr := fmt.Errorf("overlayfs require only one mount info, but get: %v", mounts)
		vm.removePlaceholder(namespace, parentSnapshotID, readyCh, mountErr)
		return nil, false, mountErr
	}
	if err = os.MkdirAll(mountPoint, common.DirMode); err != nil {
		vm.removePlaceholder(namespace, parentSnapshotID, readyCh, err)
		return nil, false, err
	}
	if err = mounts[0].Mount(mountPoint); err != nil {
		mountErr := fmt.Errorf("mount snapshot %v failed: %v", viewSnapshotKey, err)
		vm.removePlaceholder(namespace, parentSnapshotID, readyCh, mountErr)
		return nil, false, mountErr
	}

	vm.viewLock.Lock()
	placeholder.ready = nil
	vm.viewLock.Unlock()
	close(readyCh)

	return placeholder, true, nil
}

// removePlaceholder cleans up a failed placeholder ref and signals waiting goroutines.
func (vm *viewManager) removePlaceholder(namespace, parentSnapshotID string, readyCh chan struct{}, initErr error) {
	vm.viewLock.Lock()
	defer vm.viewLock.Unlock()

	if nsMap, ok := vm.viewMounts[namespace]; ok {
		if ref, ok := nsMap[parentSnapshotID]; ok && ref.ready == readyCh {
			ref.initErr = initErr
			delete(nsMap, parentSnapshotID)
			if len(nsMap) == 0 {
				delete(vm.viewMounts, namespace)
			}
		}
	}
	close(readyCh)
}

// releaseViewMount decrements ref count and performs cleanup when refCount reaches zero.
// Returns error if cleanup fails, allowing caller to handle state restoration.
func (vm *viewManager) releaseViewMount(snt snapshotter.Snapshotter, namespace, snapshotID string) error {
	vm.viewLock.Lock()
	defer vm.viewLock.Unlock()

	nsMap, ok := vm.viewMounts[namespace]
	if !ok {
		return nil
	}
	ref, ok := nsMap[snapshotID]
	if !ok || ref.ready != nil {
		return nil
	}
	if ref.refCount > 1 {
		ref.refCount--
		return nil
	}
	// Store cleanup data before deleting the ref
	refNamespace := ref.namespace
	snapshotKey := ref.snapshotKey
	mountPoint := ref.mountPoint

	// Mark ref as pending cleanup to prevent reuse during cleanup
	ref.refCount = -1
	delete(nsMap, snapshotID)
	if len(nsMap) == 0 {
		delete(vm.viewMounts, namespace)
	}

	// Perform cleanup operations and collect errors
	var cleanupErrs []error
	if unmountErr := mount.Unmount(mountPoint, unix.MNT_FORCE); unmountErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("unmount %s: %w", mountPoint, unmountErr))
	}
	if removeDirErr := os.RemoveAll(mountPoint); removeDirErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("remove dir %s: %w", mountPoint, removeDirErr))
	} else if pruneErr := cleanupEmptySnapshotParents(mountPoint); pruneErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("prune empty parent dirs for %s: %w", mountPoint, pruneErr))
	}
	if removeErr := snt.Remove(context.Background(), refNamespace, snapshotKey); removeErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("remove snapshot %s: %w", snapshotKey, removeErr))
	}

	// If cleanup failed, log and return error for caller handling
	if len(cleanupErrs) > 0 {
		slog.Warn("releaseViewMount cleanup had errors", "snapshotID", snapshotID, "errors", errors.Join(cleanupErrs...))
		return errors.Join(cleanupErrs...)
	}
	return nil
}

// CleanupAllViews unmounts and removes all view snapshots and their mount directories.
// Should be called during graceful shutdown.
func (vm *viewManager) CleanupAllViews(snt snapshotter.Snapshotter) {
	vm.viewLock.Lock()
	defer vm.viewLock.Unlock()

	// Collect all refs and clear maps in one pass.
	type viewItem struct {
		namespace   string
		snapshotKey string
		mountPoint  string
	}
	var items []viewItem
	for ns, nsMap := range vm.viewMounts {
		for _, ref := range nsMap {
			items = append(items, viewItem{
				namespace:   ref.namespace,
				snapshotKey: ref.snapshotKey,
				mountPoint:  ref.mountPoint,
			})
		}
		delete(vm.viewMounts, ns)
	}
	for ns := range vm.viewAliases {
		delete(vm.viewAliases, ns)
	}

	for _, item := range items {
		if err := mount.Unmount(item.mountPoint, unix.MNT_FORCE); err != nil {
			slog.Warn("cleanup: failed to unmount", "mountPoint", item.mountPoint, "err", err)
		}
		if err := os.RemoveAll(item.mountPoint); err != nil {
			slog.Warn("cleanup: failed to delete dir", "mountPoint", item.mountPoint, "err", err)
		} else if err := cleanupEmptySnapshotParents(item.mountPoint); err != nil {
			slog.Warn("cleanup: failed to prune empty parent dirs", "mountPoint", item.mountPoint, "err", err)
		}
		if err := snt.Remove(context.Background(), item.namespace, item.snapshotKey); err != nil {
			slog.Warn("cleanup: failed to remove view snapshot", "snapshotKey", item.snapshotKey, "err", err)
		}
	}
	if len(items) > 0 {
		slog.Info("cleanup: removed view mounts", "count", len(items))
	}
}
