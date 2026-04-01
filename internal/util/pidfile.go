package util

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func WritePIDFile(pidFile string) error {
	// Empty pidFile disables pid-file management.
	if pidFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		return fmt.Errorf("failed to create pid file directory: %w", err)
	}
	if existingPID, err := readPIDFile(pidFile); err == nil {
		running, runErr := isProcessRunning(existingPID)
		if runErr != nil {
			return fmt.Errorf("unexpected error from checking existing pid %d: %w", existingPID, runErr)
		}
		if running {
			return fmt.Errorf("pid file %s already exists and process %d is still running", pidFile, existingPID)
		}
		// If the file exists but its pid is not running, treat it as stale and replace it.
		if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove stale pid file: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		// Missing pid file is expected; other read errors should stop startup.
		return err
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

func readPIDFile(pidFile string) (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, os.ErrNotExist
		}
		return 0, fmt.Errorf("failed to read pid file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("failed to parse pid file %s: %w", pidFile, err)
	}
	return pid, nil
}

func isProcessRunning(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}
