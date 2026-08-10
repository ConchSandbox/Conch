package vmm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/openeuler/Conch/internal/config"
)

var sandboxSocketNameRE = regexp.MustCompile(`^[a-f0-9]{16}\.sock(?:\.serial)?$`)

// CleanupStaleResources terminates persisted VMM processes left behind by a
// previous daemon and removes their socket files.
func CleanupStaleResources(pids []int, binaries map[string]string, hasCreatingSandbox bool) error {
	var errs []error
	processedPIDs := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		processedPIDs[pid] = struct{}{}
		if err := killStaleVMMProcess(pid, binaries); err != nil {
			errs = append(errs, err)
		}
	}
	if hasCreatingSandbox {
		if err := cleanupUnrecordedVMMProcesses(processedPIDs, binaries); err != nil {
			errs = append(errs, err)
		}
	}
	for _, subdir := range []string{"v", "x"} {
		dir := filepath.Join(config.WorkDir, subdir)
		files, readErr := os.ReadDir(dir)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			errs = append(errs, fmt.Errorf("list stale VMM sockets in %s: %w", dir, readErr))
			continue
		}
		for _, file := range files {
			if file.IsDir() || !sandboxSocketNameRE.MatchString(file.Name()) {
				continue
			}
			if removeErr := os.Remove(filepath.Join(dir, file.Name())); removeErr != nil && !os.IsNotExist(removeErr) {
				errs = append(errs, fmt.Errorf("remove stale VMM socket %s: %w", file.Name(), removeErr))
			}
		}
	}
	return errors.Join(errs...)
}

func killStaleVMMProcess(pid int, binaries map[string]string) error {
	if pid <= 0 {
		return nil
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read stale VMM pid %d command line: %w", pid, err)
	}
	if !matchesConfiguredVMMCommand(cmdline, binaries) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill stale VMM pid %d: %w", pid, err)
	}
	return nil
}

func cleanupUnrecordedVMMProcesses(processedPIDs map[int]struct{}, binaries map[string]string) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("scan VMM processes: %w", err)
	}

	var errs []error
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}
		if _, processed := processedPIDs[pid]; processed {
			continue
		}
		cmdline, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if readErr != nil || !matchesConfiguredVMMCommand(cmdline, binaries) {
			continue
		}
		proc, findErr := os.FindProcess(pid)
		if findErr != nil {
			continue
		}
		if killErr := proc.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			errs = append(errs, fmt.Errorf("kill unrecorded VMM pid %d: %w", pid, killErr))
		}
	}
	return errors.Join(errs...)
}

func matchesConfiguredVMMCommand(cmdline []byte, binaries map[string]string) bool {
	argv0 := string(cmdline)
	if index := strings.IndexByte(argv0, 0); index >= 0 {
		argv0 = argv0[:index]
	}
	if argv0 == "" {
		return false
	}
	argv0 = filepath.Clean(argv0)
	for _, binary := range binaries {
		if binary != "" && argv0 == filepath.Clean(binary) {
			return true
		}
	}
	return false
}
