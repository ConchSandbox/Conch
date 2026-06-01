// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: Rootfs-defined startup entrypoint for conch-agent PID 1

package guestd

import (
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/openeuler/Conch/pkg/ulog"
)

const rootfsEntrypoint = "/etc/conch/entrypoint"

func hasRootfsEntrypoint() bool {
	info, err := os.Stat(MergeTarget + rootfsEntrypoint)
	return err == nil && !info.IsDir() && info.Mode()&0111 != 0
}

func startRootfsEntrypoint() bool {
	logger := ulog.GetLogger()
	if !hasRootfsEntrypoint() {
		logger.Info("Rootfs conch entrypoint not found")
		return false
	}

	cmd := exec.Command(rootfsEntrypoint)
	if chrootCommand.available() {
		cmd = execChroot(MergeTarget, rootfsEntrypoint)
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: MergeTarget}
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = "/"
	sandboxID := currentSandboxID
	if sandboxID == "" {
		sandboxID = strings.TrimSpace(getSandboxIDFromCmdline())
	}

	cmd.Env = append(os.Environ(),
		"HOME=/root",
		"PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin",
	)
	if sandboxID != "" {
		cmd.Env = append(cmd.Env, "CONCH_SANDBOX_ID="+sandboxID)
	}

	if err := cmd.Start(); err != nil {
		logger.Error("Failed to start rootfs conch entrypoint", ulog.F("error", err))
		return false
	}

	logger.Info("Started rootfs conch entrypoint", ulog.F("pid", cmd.Process.Pid))
	return true
}
