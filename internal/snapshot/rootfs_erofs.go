package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
)

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
		for _, opt := range m.Options {
			if _, ok := strings.CutPrefix(opt, "device="); ok {
				return nil, fmt.Errorf("erofs fsmerge mounts are not supported: %s", opt)
			}
		}
		if err := add(m.Source); err != nil {
			return nil, err
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
