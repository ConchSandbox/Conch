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

// CleanupStaleResources terminates Conch VMM command lines left behind by a
// previous daemon and removes their socket files. VMMs identify their owner
// in the guest kernel command line, so no PID is persisted or trusted across
// daemon restarts.
func CleanupStaleResources() error {
	var errs []error
	entries, err := os.ReadDir("/proc")
	if err == nil {
		for _, entry := range entries {
			if _, parseErr := strconv.Atoi(entry.Name()); parseErr != nil {
				continue
			}
			cmdline, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
			if readErr != nil || !isConchVMMCommand(cmdline) {
				continue
			}
			pid, _ := strconv.Atoi(entry.Name())
			proc, findErr := os.FindProcess(pid)
			if findErr != nil {
				continue
			}
			if killErr := proc.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("kill stale VMM pid %d: %w", pid, killErr))
			}
		}
	} else if !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("scan VMM processes: %w", err))
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
			if file.IsDir() || !isSandboxSocketName(file.Name()) {
				continue
			}
			if removeErr := os.Remove(filepath.Join(dir, file.Name())); removeErr != nil && !os.IsNotExist(removeErr) {
				errs = append(errs, fmt.Errorf("remove stale VMM socket %s: %w", file.Name(), removeErr))
			}
		}
	}
	return errors.Join(errs...)
}

func isSandboxSocketName(name string) bool {
	return sandboxSocketNameRE.MatchString(name)
}

func isConchVMMCommand(cmdline []byte) bool {
	command := string(cmdline)
	if !strings.Contains(command, "conch.sandbox_id=") {
		return false
	}
	if !strings.Contains(command, "stratovirt") && !strings.Contains(command, "cloud-hypervisor") {
		return false
	}
	return true
}
