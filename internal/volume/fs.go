package volume

import (
	"fmt"
	"os"
)

func mkdirAll(path string) error {
	// 0o777: volume data directories are mounted into the sandbox through
	// virtiofs and must be writable by the default unprivileged guest user
	// (uid 1000), not just the root-owned host process. MkdirAll applies the
	// process umask (typically 022 strips world-write), so the mode must be
	// re-asserted with an explicit chmod.
	if err := os.MkdirAll(path, 0o777); err != nil {
		return fmt.Errorf("create volume directory %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o777); err != nil {
		return fmt.Errorf("set volume directory permissions %s: %w", path, err)
	}
	return nil
}

func removeAll(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove volume directory %s: %w", path, err)
	}
	return nil
}
