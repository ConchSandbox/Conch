package util

import (
	sddaemon "github.com/coreos/go-systemd/v22/daemon"

	"github.com/openeuler/Conch/pkg/ulog"
)

// notifySystemd sends one sd_notify(3) message to the service manager.
// It is a no-op when conchd was not started by systemd (NOTIFY_SOCKET unset),
// so callers can notify unconditionally.
func notifySystemd(state string) {
	sent, err := sddaemon.SdNotify(false, state)
	if err != nil {
		ulog.GetLogger().Warn("Failed to notify systemd",
			ulog.F("state", state),
			ulog.F("error", err),
		)
		return
	}
	if sent {
		ulog.GetLogger().Debug("Notified systemd", ulog.F("state", state))
	}
}

// NotifyReady reports that start-up finished. A `Type=notify` unit keeps
// `systemctl start conchd` blocked until this arrives, so dependent units and
// scripts never race against a daemon that is not serving yet.
func NotifyReady() {
	notifySystemd(sddaemon.SdNotifyReady)
}

// NotifyStopping reports that shutdown began, so `systemctl status` shows
// deactivating while cleanup runs.
func NotifyStopping() {
	notifySystemd(sddaemon.SdNotifyStopping)
}
