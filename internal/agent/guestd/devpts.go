// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: devpts setup for rootfs PTY support

package guestd

import (
	"os"

	"github.com/openeuler/Conch/pkg/ulog"
)

func setupDevPts() {
	logger := ulog.GetLogger()
	devPts := MergeTarget + "/dev/pts"
	if err := os.MkdirAll(devPts, 0755); err != nil {
		logger.Error("Failed to create devpts directory", ulog.F("target", devPts), ulog.F("error", err))
		return
	}

	if isMountPoint(devPts) {
		logger.Info("devpts already mounted", ulog.F("target", devPts))
		return
	}

	if err := mountFS("devpts", devPts, "devpts", 0, ""); err != nil {
		logger.Error("Failed to mount devpts", ulog.F("target", devPts), ulog.F("error", err))
		return
	}

	logger.Info("Mounted devpts", ulog.F("target", devPts))
}
