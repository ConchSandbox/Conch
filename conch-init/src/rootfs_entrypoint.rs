use std::process::Stdio;

use tokio::fs;
use tokio::process::Command;
use tracing::{error, info, warn};

use crate::constants::MERGE_TARGET;
use crate::state::AgentState;
use crate::util::resolve_command;

pub async fn has_rootfs_entrypoint() -> bool {
    let path = format!("{MERGE_TARGET}/etc/conch/entrypoint");
    match fs::metadata(path).await {
        Ok(info) => {
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                !info.is_dir() && info.permissions().mode() & 0o111 != 0
            }
            #[cfg(not(unix))]
            {
                !info.is_dir()
            }
        }
        Err(_) => false,
    }
}

pub async fn start_rootfs_entrypoint(state: &AgentState) -> bool {
    let mut cmd = rootfs_entrypoint_command();
    cmd.current_dir("/")
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .env("HOME", "/root")
        .env("PATH", "/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin");
    let sandbox_id = state.sandbox_id();
    if !sandbox_id.is_empty() {
        cmd.env("CONCH_SANDBOX_ID", sandbox_id);
    }
    match cmd.spawn() {
        Ok(child) => {
            info!(
                pid = child.id().unwrap_or_default(),
                "Started rootfs conch entrypoint"
            );
            true
        }
        Err(err) => {
            error!(error = %err, "Failed to start rootfs conch entrypoint");
            false
        }
    }
}

fn rootfs_entrypoint_command() -> Command {
    if let Some(chroot) = resolve_command(
        "chroot",
        &["/usr/sbin/chroot", "/usr/bin/chroot", "/bin/chroot"],
    ) {
        let mut cmd = Command::new(chroot);
        cmd.arg(MERGE_TARGET).arg("/etc/conch/entrypoint");
        return cmd;
    }

    let mut cmd = Command::new("/etc/conch/entrypoint");
    #[cfg(unix)]
    {
        use std::ffi::CString;
        use std::io;
        use std::os::unix::process::CommandExt;

        let root = CString::new(MERGE_TARGET).expect("merge target contains no nul bytes");
        let cwd = CString::new("/").expect("root path contains no nul bytes");
        unsafe {
            cmd.as_std_mut().pre_exec(move || {
                if libc::chroot(root.as_ptr()) != 0 {
                    return Err(io::Error::last_os_error());
                }
                if libc::chdir(cwd.as_ptr()) != 0 {
                    return Err(io::Error::last_os_error());
                }
                Ok(())
            });
        }
    }
    cmd
}

pub async fn wait_for_rootfs_service_ready_signal(state: AgentState) {
    match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::user_defined1()) {
        Ok(mut sig) => {
            while sig.recv().await.is_some() {
                state.mark_rootfs_services_ready();
                info!("Rootfs services marked ready via SIGUSR1");
            }
        }
        Err(err) => warn!(error = %err, "Failed to register SIGUSR1 handler"),
    }
}
