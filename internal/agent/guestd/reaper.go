// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: Child process reaper for conch-agent PID 1

package guestd

import (
	"errors"
	"syscall"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
)

// reapChildren reaps zombie child processes (required for PID 1).
func reapChildren() {
	logger := ulog.GetLogger()
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) || pid == 0 {
			time.Sleep(1 * time.Second)
			continue
		}
		if err != nil {
			logger.Warn("Failed to reap child process", ulog.F("error", err))
			time.Sleep(1 * time.Second)
			continue
		}
		logger.Info("Reaped child process", ulog.F("pid", pid), ulog.F("exit_code", status.ExitStatus()))
	}
}
