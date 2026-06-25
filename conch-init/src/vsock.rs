use std::io;
use std::os::fd::RawFd;
use std::time::{Duration, Instant};

use tracing::{error, info, warn};

use crate::constants::{SANDBOX_READY_RESPONSE_TIMEOUT, SERVER_VERSION, VSOCK_READY_PORT};
use crate::state::AgentState;

pub fn start_vsock_server_async(state: AgentState) -> io::Result<()> {
    let fd = create_vsock_listener(VSOCK_READY_PORT)?;
    info!(port = VSOCK_READY_PORT, "vsock server listening");
    let runtime = tokio::runtime::Handle::current();
    tokio::task::spawn_blocking(move || {
        listen_vsock_loop(fd, state, runtime);
    });
    Ok(())
}

fn listen_vsock_loop(fd: RawFd, state: AgentState, runtime: tokio::runtime::Handle) {
    let _listener = FdGuard(fd);
    loop {
        let conn_fd = match accept_vsock_connection(fd) {
            Ok(fd) => fd,
            Err(err) => {
                error!(error = %err, "vsock accept error");
                continue;
            }
        };
        handle_vsock_connection(conn_fd, state.clone(), runtime.clone());
    }
}

fn handle_vsock_connection(conn_fd: RawFd, state: AgentState, runtime: tokio::runtime::Handle) {
    let _conn = FdGuard(conn_fd);
    let mut buf = [0_u8; 1024];
    let n = unsafe { libc::read(conn_fd, buf.as_mut_ptr().cast(), buf.len()) };
    if n < 0 {
        let err = io::Error::last_os_error();
        error!(fd = conn_fd, error = %err, "vsock read error");
        return;
    }
    if n > 0 {
        let message = String::from_utf8_lossy(&buf[..n as usize]).to_string();
        let response = runtime.block_on(handle_vsock_message(&state, &message));
        if !response.is_empty() {
            let bytes = response.as_bytes();
            let written = unsafe { libc::write(conn_fd, bytes.as_ptr().cast(), bytes.len()) };
            if written < 0 {
                error!(fd = conn_fd, error = %io::Error::last_os_error(), "vsock write response error");
            } else if response.contains("READY:") {
                info!(fd = conn_fd, "vsock sent READY response");
            }
        }
    }
}

struct FdGuard(RawFd);

impl Drop for FdGuard {
    fn drop(&mut self) {
        unsafe { libc::close(self.0) };
    }
}

pub async fn handle_vsock_message(state: &AgentState, message: &str) -> String {
    if !message.contains("SANDBOX_ID:") {
        return String::new();
    }
    let sandbox_id = parse_vsock_field(message, "SANDBOX_ID:");
    if sandbox_id.is_empty() {
        return String::new();
    }
    let agent_token = parse_vsock_field(message, "AGENT_TOKEN:");
    if agent_token.is_empty() {
        warn!("agent token missing from vsock init message");
        return "NOT_READY\n".into();
    }
    state.auth.set_token(&agent_token);
    if state.sandbox_id() != sandbox_id {
        state.set_sandbox_id(sandbox_id.clone());
        info!(new_sandbox_id = %sandbox_id, "Updated sandbox_id from vsock");
    }

    if wait_for_ready(state, SANDBOX_READY_RESPONSE_TIMEOUT).await {
        info!(
            version = SERVER_VERSION,
            "sandbox services healthy, sent READY back with version"
        );
        format!("OK\nREADY:{SERVER_VERSION}\n")
    } else {
        warn!(timeout = ?SANDBOX_READY_RESPONSE_TIMEOUT, "sandbox services not ready before vsock response timeout");
        "NOT_READY\n".into()
    }
}

pub fn parse_vsock_field(message: &str, prefix: &str) -> String {
    message
        .lines()
        .find_map(|line| {
            line.trim()
                .split_once(prefix)
                .map(|(_, v)| v.trim().to_string())
        })
        .unwrap_or_default()
}

async fn wait_for_ready(state: &AgentState, timeout: Duration) -> bool {
    if state.check_sandbox_ready().await {
        return true;
    }
    let deadline = Instant::now() + timeout;
    loop {
        if state.check_sandbox_ready().await {
            return true;
        }
        let now = Instant::now();
        if now >= deadline {
            return state.check_sandbox_ready().await;
        }
        tokio::select! {
            _ = state.grpc_ready_notify.notified() => {}
            _ = state.rootfs_services_ready_notify.notified() => {}
            _ = tokio::time::sleep(deadline - now) => {
                return state.check_sandbox_ready().await;
            }
        }
    }
}

fn create_vsock_listener(port: u32) -> io::Result<RawFd> {
    let fd = unsafe { libc::socket(libc::AF_VSOCK, libc::SOCK_STREAM, 0) };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    let sa = libc::sockaddr_vm {
        svm_family: libc::AF_VSOCK as libc::sa_family_t,
        svm_reserved1: 0,
        svm_port: port,
        svm_cid: libc::VMADDR_CID_ANY,
        svm_zero: [0; 4],
    };
    let ret = unsafe {
        libc::bind(
            fd,
            (&sa as *const libc::sockaddr_vm).cast(),
            std::mem::size_of::<libc::sockaddr_vm>() as libc::socklen_t,
        )
    };
    if ret < 0 {
        let err = io::Error::last_os_error();
        unsafe { libc::close(fd) };
        return Err(err);
    }
    if unsafe { libc::listen(fd, 5) } < 0 {
        let err = io::Error::last_os_error();
        unsafe { libc::close(fd) };
        return Err(err);
    }
    Ok(fd)
}

fn accept_vsock_connection(fd: RawFd) -> io::Result<RawFd> {
    let nfd = unsafe { libc::accept(fd, std::ptr::null_mut(), std::ptr::null_mut()) };
    if nfd < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(nfd)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::constants::AGENT_TOKEN_METADATA_KEY;
    use tonic::metadata::MetadataMap;

    #[test]
    fn parse_vsock_field_extracts_trimmed_value() {
        let message = "I AM SANDBOX_ID:sandbox-1\nAGENT_TOKEN: secret \n";
        assert_eq!(parse_vsock_field(message, "SANDBOX_ID:"), "sandbox-1");
        assert_eq!(parse_vsock_field(message, "AGENT_TOKEN:"), "secret");
    }

    #[tokio::test]
    async fn vsock_handler_requires_agent_token() {
        let state = AgentState::new();
        state.mark_grpc_ready();

        let response = handle_vsock_message(&state, "I AM SANDBOX_ID:sandbox-1\n").await;
        assert_eq!(response, "NOT_READY\n");
    }

    #[tokio::test]
    async fn vsock_handler_sets_agent_token() {
        let state = AgentState::new();
        state.mark_grpc_ready();

        let response =
            handle_vsock_message(&state, "I AM SANDBOX_ID:sandbox-1\nAGENT_TOKEN:secret\n").await;
        assert_eq!(response, "OK\nREADY:0.0.4\n");

        let mut metadata = MetadataMap::new();
        metadata.insert(AGENT_TOKEN_METADATA_KEY, "secret".parse().unwrap());
        assert!(
            state
                .auth
                .verify_value(metadata.get(AGENT_TOKEN_METADATA_KEY))
                .is_ok()
        );
    }
}
