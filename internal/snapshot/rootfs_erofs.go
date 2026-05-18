package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
)

func (s *server) resolveRootfsPmemFiles(ctx context.Context, namespace, rootfsKey string) ([]string, error) {
	if s.rootfsSnt == nil {
		return nil, fmt.Errorf("rootfs erofs snapshotter is not configured")
	}
	mounts, err := s.rootfsSnt.Mounts(ctx, namespace, rootfsKey)
	if err != nil {
		return nil, fmt.Errorf("resolve erofs rootfs mounts for %s: %w", rootfsKey, err)
	}
	return pmemFilesFromErofsMounts(mounts)
}

func pmemFilesFromErofsMounts(mounts []mount.Mount) ([]string, error) {
	var files []string
	seen := make(map[string]struct{})
	add := func(path string) error {
		path = strings.TrimSpace(path)
		if path == "" {
			return nil
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("erofs pmem path is not absolute: %s", path)
		}
		if _, ok := seen[path]; ok {
			return nil
		}
		seen[path] = struct{}{}
		files = append(files, path)
		return nil
	}

	for _, m := range mounts {
		if m.Type != "erofs" {
			continue
		}
		if err := add(m.Source); err != nil {
			return nil, err
		}
		for _, opt := range m.Options {
			if device, ok := strings.CutPrefix(opt, "device="); ok {
				if err := add(device); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("erofs rootfs mounts contain no pmem files")
	}
	return files, nil
}

func alignRootfsPmemFiles(files []string) error {
	const align = int64(2 * 1024 * 1024)
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat rootfs pmem %s: %w", path, err)
		}
		size := info.Size()
		if size <= 0 || size%align == 0 {
			continue
		}
		aligned := ((size + align - 1) / align) * align
		if err := os.Truncate(path, aligned); err != nil {
			return fmt.Errorf("align rootfs pmem %s from %d to %d: %w", path, size, aligned, err)
		}
	}
	return nil
}

func selectSnapshotRestorePmemFiles(files []string, deviceCount int) ([]string, error) {
	if deviceCount <= 0 || len(files) == deviceCount {
		return files, nil
	}
	if len(files) < deviceCount {
		return nil, fmt.Errorf("rootfs pmem file count %d is less than snapshot device count %d", len(files), deviceCount)
	}
	return files[len(files)-deviceCount:], nil
}
