package snapshot

import (
	"context"
	"log/slog"
	"os"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/snapshot/snapshotter"
)

// snapshotCleaner handles cleanup of snapshot resources.
type snapshotCleaner struct {
	ctx        context.Context
	viewMgr    *viewManager
	snt        snapshotter.Snapshotter
	mountMgr   mount.Manager
	namespace  string
	key        string
	mountPoint string
	accessPath string

	prepared   bool
	viewed     bool
	dirCreated bool
	mounted    bool

	parentSnapshotID string
	activationKey    string
}

// Cleanup releases all resources held by the snapshot.
func (sc *snapshotCleaner) Cleanup() {
	if sc.viewed {
		if sc.viewMgr != nil {
			sc.viewMgr.removeViewAlias(sc.namespace, sc.key)
			sc.viewMgr.releaseViewMount(sc.snt, sc.mountMgr, sc.namespace, sc.parentSnapshotID)
		}
		return
	}

	if sc.mounted {
		if unmountErr := mount.UnmountAll(sc.mountPoint, unix.MNT_FORCE); unmountErr != nil {
			slog.Warn("failed to unmount", "mountPoint", sc.mountPoint, "err", unmountErr)
		}
	}
	if sc.activationKey != "" && sc.mountMgr != nil {
		ctx := namespaces.WithNamespace(sc.ctx, sc.namespace)
		if deactivateErr := sc.mountMgr.Deactivate(ctx, sc.activationKey); deactivateErr != nil {
			if !isMountCleanupNotFound(deactivateErr) {
				slog.Warn("failed to deactivate mount", "key", sc.activationKey, "err", deactivateErr)
			}
		}
	}
	if sc.dirCreated {
		if removeDirErr := os.RemoveAll(sc.mountPoint); removeDirErr != nil {
			slog.Warn("failed to delete dir", "mountPoint", sc.mountPoint, "err", removeDirErr)
		} else if pruneErr := cleanupEmptySnapshotParents(sc.mountPoint); pruneErr != nil {
			slog.Warn("failed to prune empty parent dirs", "mountPoint", sc.mountPoint, "err", pruneErr)
		}
	}
	if sc.prepared {
		if sc.snt != nil {
			if removeErr := sc.snt.Remove(sc.ctx, sc.namespace, sc.key); removeErr != nil {
				slog.Warn("failed to remove snapshot", "key", sc.key, "err", removeErr)
			}
		}
	}
}
