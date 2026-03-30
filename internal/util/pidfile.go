package util

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func WritePIDFile(pidFile string) error {
	if pidFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		return fmt.Errorf("failed to create pid file directory: %w", err)
	}
	pid := strconv.Itoa(os.Getpid()) + "\n"
	if err := os.WriteFile(pidFile, []byte(pid), 0o644); err != nil {
		return fmt.Errorf("failed to write pid file: %w", err)
	}
	return nil
}

func RemovePIDFile(pidFile string) {
	if pidFile == "" {
		return
	}
	_ = os.Remove(pidFile)
}
