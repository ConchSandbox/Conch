package snapshot

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/containerd/containerd/mount"
)

type snapshotCleaner struct {
	ctx        context.Context
	server     *server
	namespace  string
	key        string
	mountPoint string

	prepared   bool
	dirCreated bool
	mounted    bool
}

func (sc *snapshotCleaner) Cleanup() {
	if sc.mounted {
		if unmountErr := mount.Unmount(sc.mountPoint, unix.MNT_FORCE); unmountErr != nil {
			fmt.Printf("failed to unmount %v: %v \n", sc.mountPoint, unmountErr)
		}
	}
	if sc.dirCreated {
		if removeDirErr := os.RemoveAll(sc.mountPoint); removeDirErr != nil {
			fmt.Printf("failed to delete dir %v: %v \n", sc.mountPoint, removeDirErr)
		}
	}
	if sc.prepared {
		if removeErr := sc.server.snt.Remove(sc.ctx, sc.namespace, sc.key); removeErr != nil {
			fmt.Printf("failed to remove snapshot %v: %v \n", sc.key, removeErr)
		}
	}
}
