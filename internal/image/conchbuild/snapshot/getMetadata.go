package snapshot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
)

type SnapshotMeta struct {
	Key         string
	Parent      string            // Info.Parent
	Labels      map[string]string // Info.Labels
	StoragePath string
}

func GetSnapshotInfo(ctx context.Context, client *containerd.Client, key string) (*SnapshotMeta, error) {
	// Inject the namespace into the context; required for containerd service calls
	ctx = namespaces.WithNamespace(ctx, "default")
	sn := client.SnapshotService("overlayfs")

	// 1. Fetch basic metadata (Stat contains Name, Parent, Labels, etc.)
	stat, err := sn.Stat(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("stat failed for key %s: %w", key, err)
	}

	// 2. Obtain mount information
	// Prioritize the lightweight Mounts call to retrieve existing mount configurations
	mounts, err := sn.Mounts(ctx, key)

	// If Mounts fails or returns empty Options, fallback to creating a temporary View
	if err != nil || len(mounts) == 0 || len(mounts[0].Options) == 0 {
		viewID := fmt.Sprintf("tmp-v-%d-%s", time.Now().UnixNano(), key)
		mounts, err = sn.View(ctx, viewID, key)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve storage path via mounts or view: %w", err)
		}
		// Ensure the temporary view is removed immediately after use
		defer sn.Remove(ctx, viewID)
	}

	// 3. Parse the physical storage path from mount options
	var p string
	if len(mounts) > 0 {
		// Iterate through options to find the real physical directory
		// We check Options first because Source is often just the string "overlay"
		for _, opt := range mounts[0].Options {
			if strings.HasPrefix(opt, "upperdir=") {
				// For Active snapshots (Rootfs), upperdir points to the specific data path
				p = strings.TrimPrefix(opt, "upperdir=")
				break
			} else if strings.HasPrefix(opt, "lowerdir=") && p == "" {
				// For Committed snapshots (Parent), use the first entry in the lowerdir stack
				p = strings.Split(strings.TrimPrefix(opt, "lowerdir="), ":")[0]
			}
		}

		// Fallback: If options yielded no path, check Source but ignore the generic string "overlay"
		if p == "" || p == "overlay" {
			p = mounts[0].Source
		}
	}

	return &SnapshotMeta{
		Key:         stat.Name,
		Parent:      stat.Parent,
		Labels:      stat.Labels,
		StoragePath: p,
	}, nil
}
