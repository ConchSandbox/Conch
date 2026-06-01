// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: Filesystem mount logic for conch-agent PID 1

package guestd

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
	"golang.org/x/sys/unix"
)

func mountFS(source, target, fstype string, flags uintptr, data string, args ...string) error {
	var commandErr error
	if mountCommand.available() && len(args) > 0 {
		if err := execMount(args...).Run(); err == nil {
			return nil
		} else {
			commandErr = err
		}
	}
	if err := syscall.Mount(source, target, fstype, flags, data); err != nil {
		if commandErr != nil {
			ulog.GetLogger().Warn("Mount command failed before syscall fallback",
				ulog.F("target", target), ulog.F("fstype", fstype),
				ulog.F("command_error", commandErr), ulog.F("syscall_error", err))
		}
		return err
	}
	return nil
}

// mountEssentialFilesystems mounts /proc, /sys, /tmp, and /dev.
func mountEssentialFilesystems() {
	logger := ulog.GetLogger()
	mounts := []struct {
		fstype string
		target string
	}{
		{"proc", "/proc"},
		{"sysfs", "/sys"},
		{"tmpfs", "/tmp"},
		{"devtmpfs", "/dev"},
	}

	for _, m := range mounts {
		os.MkdirAll(m.target, 0755)
		if err := mountFS("none", m.target, m.fstype, 0, "", "-t", m.fstype, "none", m.target); err != nil {
			logger.Error("Failed to mount filesystem", ulog.F("fstype", m.fstype), ulog.F("target", m.target), ulog.F("error", err))
		} else {
			if m.target == "/proc" {
				refreshSandboxLoggerFromCmdline()
				logger = ulog.GetLogger()
			}
			logger.Info("Mounted filesystem", ulog.F("fstype", m.fstype), ulog.F("target", m.target))
		}
	}
}

// mountStorageDevices mounts the writable layer and pmem devices, then sets up OverlayFS.
func mountStorageDevices() {
	logger := ulog.GetLogger()
	// Create vda device node
	if _, err := os.Stat("/dev/vda"); os.IsNotExist(err) {
		if err := syscall.Mknod("/dev/vda", syscall.S_IFBLK|0600, int(unix.Mkdev(253, 0))); err != nil {
			logger.Warn("Failed to create /dev/vda", ulog.F("error", err))
		}
	}

	// Try to mount vda
	upperDir := "/mnt/conch/upper"
	workDir := "/mnt/conch/work"

	os.MkdirAll("/mnt/disk", 0755)
	if err := mountFS("/dev/vda", "/mnt/disk", "ext4", 0, "", "-t", "ext4", "/dev/vda", "/mnt/disk"); err != nil {
		logger.Info("Using RAM for writable layer")
		os.MkdirAll("/mnt/conch/upper", 0755)
		os.MkdirAll("/mnt/conch/work", 0755)
	} else {
		logger.Info("Persistent disk mounted", ulog.F("device", "/dev/vda"))
		upperDir = "/mnt/disk/upper"
		workDir = "/mnt/disk/work"
		os.MkdirAll(upperDir, 0755)
		os.MkdirAll(workDir, 0755)
	}

	// Prepare merge point
	os.MkdirAll("/mnt/conch/merge", 0755)

	// Scan and mount pmem devices
	lowerDirs := mountPmemDevices()

	// Mount OverlayFS
	if len(lowerDirs) > 0 {
		mountOverlayFS(lowerDirs, upperDir, workDir)
	}
}

// mountPmemDevices scans and mounts pmem devices
func mountPmemDevices() string {
	var entries []string
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		entries, _ = filepath.Glob("/dev/pmem*")
		if len(entries) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(entries) == 0 {
		ulog.GetLogger().Warn("No pmem devices found")
		return ""
	}

	var lowerDirs []string
	logger := ulog.GetLogger()
	for _, device := range entries {
		devName := filepath.Base(device)
		if devName == "" {
			continue
		}

		mountPoint := "/mnt/conch/" + devName
		os.MkdirAll(mountPoint, 0755)

		if err := mountFS(device, mountPoint, "erofs", syscall.MS_RDONLY, "", "-t", "erofs", "-o", "ro", device, mountPoint); err != nil {
			logger.Error("Failed to mount pmem device", ulog.F("device", device), ulog.F("target", mountPoint), ulog.F("error", err))
			continue
		}

		if lowerDirs == nil {
			lowerDirs = []string{mountPoint}
		} else {
			lowerDirs = append([]string{mountPoint}, lowerDirs...)
		}
		logger.Info("Mounted pmem device", ulog.F("device", device), ulog.F("target", mountPoint))
	}

	return strings.Join(lowerDirs, ":")
}

// mountOverlayFS mounts the OverlayFS merge layer
func mountOverlayFS(lowerDirs, upperDir, workDir string) {
	logger := ulog.GetLogger()
	opts := "lowerdir=" + lowerDirs + ",upperdir=" + upperDir + ",workdir=" + workDir
	if err := mountFS("overlay", MergeTarget, "overlay", 0, opts, "-t", "overlay", "overlay", "-o", opts, MergeTarget); err != nil {
		logger.Error("Failed to mount OverlayFS", ulog.F("target", MergeTarget), ulog.F("error", err))
	} else {
		logger.Info("Mounted OverlayFS", ulog.F("target", MergeTarget))
	}
}

// prepareMergeRoot ensures /root exists and root's home directory is /root.
func prepareMergeRoot() {
	logger := ulog.GetLogger()
	os.MkdirAll(MergeTarget+"/root", 0755)

	// Ensure root's home is /root without corrupting an already-correct passwd line.
	passwdFile := MergeTarget + "/etc/passwd"
	if _, err := os.Stat(passwdFile); err == nil {
		content, _ := os.ReadFile(passwdFile)
		var out []string
		scanner := bufio.NewScanner(strings.NewReader(string(content)))
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "root:") {
				parts := strings.Split(line, ":")
				if len(parts) >= 7 {
					parts[5] = "/root"
					line = strings.Join(parts[:7], ":")
				}
			}
			out = append(out, line)
		}
		_ = os.WriteFile(passwdFile, []byte(strings.Join(out, "\n")+"\n"), 0644)
		logger.Info("Ensured /etc/passwd root home is /root")
	}
}

// bindMountToMerge bind-mounts host filesystems into the OverlayFS merge layer.
func bindMountToMerge() {
	logger := ulog.GetLogger()
	for _, dir := range []string{"/proc", "/sys", "/dev", "/tmp"} {
		target := MergeTarget + dir
		os.MkdirAll(target, 0755)
		if err := mountFS(dir, target, "", syscall.MS_BIND, "", "--bind", dir, target); err != nil {
			logger.Error("Failed to bind mount", ulog.F("source", dir), ulog.F("target", target), ulog.F("error", err))
		} else {
			logger.Info("Bind mounted path", ulog.F("source", dir), ulog.F("target", target))
		}
	}
}
