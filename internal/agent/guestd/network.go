// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: Network configuration for conch-agent PID 1

package guestd

import (
	"strings"

	"github.com/openeuler/Conch/pkg/ulog"
)

// setupNetwork replicates the original conch-network-setup.sh script
func setupNetwork() {
	logger := ulog.GetLogger()
	// ip link set lo up
	if err := execIP("link", "set", "lo", "up").Run(); err != nil {
		logger.Warn("Failed to bring loopback up", ulog.F("error", err))
	}

	// Get first non-lo interface
	out, err := execIP("-br", "link", "show").Output()
	if err != nil {
		logger.Error("Failed to get network interfaces", ulog.F("error", err))
		return
	}

	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) > 0 {
			nicName := parts[0]
			if nicName == "lo" {
				continue
			}
			logger.Info("Configuring interface", ulog.F("name", nicName))

			// ip addr add 192.168.100.21/24 dev $NIC_NAME
			if err := execIP("addr", "add", "192.168.100.21/24", "dev", nicName).Run(); err != nil {
				logger.Warn("Failed to assign address", ulog.F("name", nicName), ulog.F("error", err))
			}
			// ip link set $NIC_NAME up
			if err := execIP("link", "set", nicName, "up").Run(); err != nil {
				logger.Warn("Failed to bring interface up", ulog.F("name", nicName), ulog.F("error", err))
			}
			// ip route add default via 192.168.100.2 dev $NIC_NAME
			if err := execIP("route", "add", "default", "via", "192.168.100.2", "dev", nicName).Run(); err != nil {
				logger.Warn("Failed to add default route", ulog.F("name", nicName), ulog.F("error", err))
			}

			// Add route for MMDS (169.254.169.254)
			if err := execIP("route", "add", "169.254.169.254/32", "dev", nicName).Run(); err != nil {
				logger.Warn("Failed to add MMDS route", ulog.F("name", nicName), ulog.F("error", err))
			}

			logger.Info("Network configured", ulog.F("name", nicName))
			return
		}
	}
}
