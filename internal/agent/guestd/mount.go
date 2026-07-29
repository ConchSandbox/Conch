// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: Filesystem mount logic for conch-init PID 1

package guestd

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
	"golang.org/x/sys/unix"
)

func mountFS(source, target, fstype string, flags uintptr, data string) error {
	if err := syscall.Mount(source, target, fstype, flags, data); err != nil {
		return err
	}
	return nil
}

func isMountPoint(target string) bool {
	target = filepath.Clean(target)

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}

	lines := splitLines(string(data))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[4] == target {
			return true
		}
	}

	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if start < i {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
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
		if err := mountFS("none", m.target, m.fstype, 0, ""); err != nil {
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
	if err := mountFS("/dev/vda", "/mnt/disk", "ext4", 0, ""); err != nil {
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

		if err := mountFS(device, mountPoint, "erofs", syscall.MS_RDONLY, ""); err != nil {
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
	if err := mountFS("overlay", MergeTarget, "overlay", 0, opts); err != nil {
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
		if err := mountFS(dir, target, "", syscall.MS_BIND, ""); err != nil {
			logger.Error("Failed to bind mount", ulog.F("source", dir), ulog.F("target", target), ulog.F("error", err))
		} else {
			logger.Info("Bind mounted path", ulog.F("source", dir), ulog.F("target", target))
		}
	}
}

const (
	conchSharefsPrefix = "conch.sharefs="
	conchfsTag         = "conchfs"
	conchfsMountPoint  = "/run/conch/volume"
	conchfsConfigFile  = "config.json"
)

type conchVolumeConfig struct {
	Version int                `json:"version"`
	Mounts  []conchVolumeMount `json:"mounts"`
}

type conchVolumeMount struct {
	Index    int    `json:"index"`
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

// mountConfiguredVolumesOrAbort mounts the single virtiofs shared dir exported
// by conchd, reads its config.json, and bind-mounts each declared volume into
// the OverlayFS merge layer so the volumes are visible post-chroot at the
// user-declared Path. Volume mounts are part of sandbox creation correctness:
// if any step fails, completed mounts are rolled back and PID 1 exits so the
// host observes sandbox startup failure. If the cmdline does not carry the
// sharefs switch, the sandbox has no volumes and the whole path is skipped.
func mountConfiguredVolumesOrAbort() {
	logger := ulog.GetLogger()
	if !sharefsEnabled() {
		return
	}
	logger.Info("conch sharefs enabled, preparing virtiofs shared dir")
	if err := os.MkdirAll(conchfsMountPoint, 0755); err != nil {
		logger.Error("Failed to create virtiofs mount point",
			ulog.F("target", conchfsMountPoint), ulog.F("error", err))
		os.Exit(1)
	}
	// Mount the single virtiofs shared dir exported by conchd. (virtiofsd 1.13.x
	// has no host cache flag; the guest uses the virtiofs default cache mode.)
	if err := mountFS(conchfsTag, conchfsMountPoint, "virtiofs", 0, ""); err != nil {
		logger.Error("Failed to mount virtiofs shared",
			ulog.F("tag", conchfsTag), ulog.F("target", conchfsMountPoint), ulog.F("error", err))
		os.Exit(1)
	}
	logger.Info("virtiofs shared dir mounted", ulog.F("target", conchfsMountPoint))

	cfgPath := filepath.Join(conchfsMountPoint, conchfsConfigFile)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		logger.Error("Failed to read volume config.json", ulog.F("path", cfgPath), ulog.F("error", err))
		os.Exit(1)
	}
	var cfg conchVolumeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		logger.Error("Failed to parse volume config.json", ulog.F("error", err))
		os.Exit(1)
	}
	logger.Info("volume config loaded", ulog.F("mounts", len(cfg.Mounts)))
	if len(cfg.Mounts) == 0 {
		return
	}

	var mountedTargets []string
	rollback := func() {
		for i := len(mountedTargets) - 1; i >= 0; i-- {
			target := mountedTargets[i]
			if err := syscall.Unmount(target, 0); err != nil {
				logger.Warn("Failed to rollback mounted volume", ulog.F("target", target), ulog.F("error", err))
			}
		}
	}
	abort := func(message string, fields ...ulog.Field) {
		logger.Error(message, fields...)
		rollback()
		os.Exit(1)
	}
	for _, mount := range cfg.Mounts {
		target := filepath.Clean(mount.Path)
		if !filepath.IsAbs(target) || isBlockedVolumeTarget(target) {
			abort("Invalid volume mount config", ulog.F("path", mount.Path), ulog.F("index", mount.Index))
		}
		mergeTarget := filepath.Join(MergeTarget, strings.TrimPrefix(target, "/"))
		if err := os.MkdirAll(mergeTarget, 0755); err != nil {
			abort("Failed to create volume mount target", ulog.F("target", mergeTarget), ulog.F("error", err))
		}
		source := filepath.Join(conchfsMountPoint, strconv.Itoa(mount.Index))
		if err := mountFS(source, mergeTarget, "", syscall.MS_BIND, ""); err != nil {
			abort("Failed to bind volume", ulog.F("source", source), ulog.F("target", mergeTarget), ulog.F("error", err))
		}
		if mount.Readonly {
			if err := mountFS("none", mergeTarget, "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
				abort("Failed to remount volume readonly", ulog.F("target", mergeTarget), ulog.F("error", err))
			}
		}
		mountedTargets = append(mountedTargets, mergeTarget)
		logger.Info("Mounted volume", ulog.F("source", source), ulog.F("target", mergeTarget), ulog.F("readonly", mount.Readonly))
	}
}

func sharefsEnabled() bool {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	for _, field := range strings.Fields(string(data)) {
		if strings.HasPrefix(field, conchSharefsPrefix) {
			return strings.TrimPrefix(field, conchSharefsPrefix) == "virtiofs"
		}
	}
	return false
}

func isBlockedVolumeTarget(target string) bool {
	switch target {
	case "/", "/proc", "/sys", "/dev", "/run":
		return true
	}
	return strings.HasPrefix(target, "/proc/") ||
		strings.HasPrefix(target, "/sys/") ||
		strings.HasPrefix(target, "/dev/") ||
		strings.HasPrefix(target, "/run/")
}
