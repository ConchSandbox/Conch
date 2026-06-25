use std::time::Duration;

use tracing::info;

pub async fn reap_children() {
    loop {
        let mut status = 0;
        let pid = unsafe { libc::waitpid(-1, &mut status, libc::WNOHANG) };
        if pid > 0 {
            info!(pid, exit_code = status, "Reaped child process");
        } else {
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
    }
}
