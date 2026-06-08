package snapshot

import (
	"github.com/containerd/containerd/v2/core/mount"
)

// IsMountPoint reports whether path is an exact mount point.
func IsMountPoint(path string) bool {
	resolved, err := mount.CanonicalizePath(path)
	if err != nil {
		return false
	}
	info, err := mount.Lookup(resolved)
	if err != nil {
		return false
	}
	return info.Mountpoint == resolved
}
