use tracing::{info, warn};

use crate::util::run_command_warn;

pub async fn setup_network() {
    run_command_warn(
        "ip",
        &["link", "set", "lo", "up"],
        "Failed to bring loopback up",
    )
    .await;
    let iface = std::fs::read_dir("/sys/class/net")
        .ok()
        .and_then(|entries| {
            entries
                .flatten()
                .map(|entry| entry.file_name().to_string_lossy().to_string())
                .find(|name| name != "lo")
        });
    let Some(nic_name) = iface else {
        warn!("No non-loopback network interface found");
        return;
    };
    info!(name = %nic_name, "Configuring interface");
    run_command_warn(
        "ip",
        &["addr", "add", "192.168.100.21/24", "dev", &nic_name],
        "Failed to assign address",
    )
    .await;
    run_command_warn(
        "ip",
        &["link", "set", &nic_name, "up"],
        "Failed to bring interface up",
    )
    .await;
    run_command_warn(
        "ip",
        &[
            "route",
            "add",
            "default",
            "via",
            "192.168.100.2",
            "dev",
            &nic_name,
        ],
        "Failed to add default route",
    )
    .await;
    run_command_warn(
        "ip",
        &["route", "add", "169.254.169.254/32", "dev", &nic_name],
        "Failed to add MMDS route",
    )
    .await;
}
