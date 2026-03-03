package snapshot

import (
	"context"
	"log/slog"
	"os"

	"github.com/containerd/containerd/mount"
	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/snapshot/snapshotter"
)

// snapshotCleaner handles cleanup of snapshot resources.
type snapshotCleaner struct {
	ctx        context.Context
	server     *server
	viewMgr    *viewManager
	snt        snapshotter.Snapshotter
	namespace  string
	key        string
	mountPoint string

	prepared   bool
	viewed     bool
	dirCreated bool
	mounted    bool

	parentSnapshotID string
}

// Cleanup releases all resources held by the snapshot.
func (sc *snapshotCleaner) Cleanup() {
	if sc.viewed {
		if sc.viewMgr != nil {
			sc.viewMgr.removeViewAlias(sc.namespace, sc.key)
			sc.viewMgr.releaseViewMount(sc.snt, sc.namespace, sc.parentSnapshotID)
		}
		return
	}

	if sc.mounted {
		if unmountErr := mount.Unmount(sc.mountPoint, unix.MNT_FORCE); unmountErr != nil {
			slog.Warn("failed to unmount", "mountPoint", sc.mountPoint, "err", unmountErr)
		}
	}
	if sc.dirCreated {
		if removeDirErr := os.RemoveAll(sc.mountPoint); removeDirErr != nil {
			slog.Warn("failed to delete dir", "mountPoint", sc.mountPoint, "err", removeDirErr)
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
