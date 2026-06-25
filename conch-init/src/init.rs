use std::env;
use std::sync::atomic::Ordering;

use tokio::fs;
use tracing::{error, info, warn};

use crate::constants::MERGE_TARGET;
use crate::devpts::setup_dev_pts;
use crate::grpc::start_grpc_server_async;
use crate::logging;
use crate::mount::{
    bind_mount_to_merge, create_dev_null, ensure_proc_mounted, mount_essential_filesystems,
    mount_storage_devices, prepare_merge_root,
};
use crate::network::setup_network;
use crate::reaper::reap_children;
use crate::rootfs_entrypoint::{
    has_rootfs_entrypoint, start_rootfs_entrypoint, wait_for_rootfs_service_ready_signal,
};
use crate::state::AgentState;
use crate::util::{chroot_to, get_sandbox_id_from_cmdline};
use crate::vsock::start_vsock_server_async;

const INIT_LOG_PATH: &str = "/var/log/conch-init/conch-init.log";

pub async fn run_as_init(state: AgentState) -> Result<(), Box<dyn std::error::Error>> {
    unsafe {
        env::set_var("PATH", "/sbin:/bin:/usr/sbin:/usr/bin");
    }
    ensure_proc_mounted().await;
    if let Some(id) = get_sandbox_id_from_cmdline() {
        state.set_sandbox_id(id);
    }
    info!(pid = unsafe { libc::getpid() }, sandbox_id = %state.sandbox_id(), "Starting conch-init as init process");

    create_dev_null().await;
    mount_essential_filesystems().await;
    info!("Using console logging before rootfs log is available");
    mount_storage_devices().await;
    setup_network().await;

    let merge_ready = fs::metadata(format!("{MERGE_TARGET}/usr")).await.is_ok();
    if merge_ready {
        prepare_merge_root().await;
        bind_mount_to_merge().await;
        setup_dev_pts().await;
        setup_merge_file_logging().await;

        let expected = has_rootfs_entrypoint().await;
        state
            .rootfs_entrypoint_expected
            .store(expected, Ordering::SeqCst);
        if expected {
            info!("Rootfs conch entrypoint found; using rootfs service startup");
            start_rootfs_entrypoint(&state).await;
        } else {
            info!("Rootfs conch entrypoint not found; skipping rootfs service startup");
        }

        if let Err(err) = chroot_to(MERGE_TARGET) {
            error!(error = %err, "Failed to chroot into merge layer, aborting init");
            return Ok(());
        }
    } else {
        warn!(target = MERGE_TARGET, "Overlay rootfs not found");
    }

    if let Err(err) = start_grpc_server_async(state.clone()).await {
        error!(error = %err, "gRPC server failed to start, vsock will report NOT_READY");
    }

    if let Err(err) = start_vsock_server_async(state.clone()) {
        error!(error = %err, "vsock server failed to start");
    }
    tokio::spawn(reap_children());
    tokio::spawn(wait_for_rootfs_service_ready_signal(state.clone()));

    info!("Initialization complete");
    wait_for_shutdown_signal().await;
    Ok(())
}

async fn setup_merge_file_logging() {
    let log_path = format!("{MERGE_TARGET}{INIT_LOG_PATH}");
    match logging::use_file(std::path::Path::new(&log_path)) {
        Ok(()) => info!(path = INIT_LOG_PATH, "Using rootfs log file"),
        Err(err) => warn!(path = %log_path, error = %err, "Failed to switch to rootfs log file"),
    }
}

async fn wait_for_shutdown_signal() {
    let mut sigterm = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        .expect("SIGTERM handler");
    let mut sigint = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::interrupt())
        .expect("SIGINT handler");
    tokio::select! {
        _ = sigterm.recv() => {}
        _ = sigint.recv() => {}
    }
    info!("Received shutdown signal");
}
