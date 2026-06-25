use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, RwLock};

use tokio::fs;
use tokio::sync::Notify;

use crate::auth::AgentAuth;
use crate::constants::{MERGE_TARGET, ROOTFS_SERVICES_READY_PATH};
use crate::logging;

#[derive(Clone)]
pub struct AgentState {
    pub auth: Arc<AgentAuth>,
    pub grpc_ready: Arc<AtomicBool>,
    pub grpc_ready_notify: Arc<Notify>,
    pub rootfs_services_ready: Arc<AtomicBool>,
    pub rootfs_services_ready_notify: Arc<Notify>,
    pub rootfs_entrypoint_expected: Arc<AtomicBool>,
    pub safe: Arc<AtomicBool>,
    sandbox_id: Arc<RwLock<String>>,
}

impl AgentState {
    pub fn new() -> Self {
        Self {
            auth: Arc::new(AgentAuth::default()),
            grpc_ready: Arc::new(AtomicBool::new(false)),
            grpc_ready_notify: Arc::new(Notify::new()),
            rootfs_services_ready: Arc::new(AtomicBool::new(false)),
            rootfs_services_ready_notify: Arc::new(Notify::new()),
            rootfs_entrypoint_expected: Arc::new(AtomicBool::new(false)),
            safe: Arc::new(AtomicBool::new(true)),
            sandbox_id: Arc::new(RwLock::new(String::new())),
        }
    }

    pub fn set_sandbox_id(&self, sandbox_id: String) {
        if let Ok(mut guard) = self.sandbox_id.write() {
            *guard = sandbox_id.clone();
        }
        logging::set_sandbox_id(&sandbox_id);
    }

    pub fn sandbox_id(&self) -> String {
        self.sandbox_id
            .read()
            .map(|v| v.clone())
            .unwrap_or_default()
    }

    pub fn mark_grpc_ready(&self) {
        self.grpc_ready.store(true, Ordering::SeqCst);
        self.grpc_ready_notify.notify_waiters();
    }

    pub fn mark_grpc_not_ready(&self) {
        self.grpc_ready.store(false, Ordering::SeqCst);
    }

    pub fn mark_rootfs_services_ready(&self) {
        self.rootfs_services_ready.store(true, Ordering::SeqCst);
        self.rootfs_services_ready_notify.notify_waiters();
    }

    pub fn check_grpc_health(&self) -> bool {
        self.safe.load(Ordering::SeqCst) && self.grpc_ready.load(Ordering::SeqCst)
    }

    pub async fn check_sandbox_ready(&self) -> bool {
        if !self.check_grpc_health() {
            return false;
        }
        !self.rootfs_services_required() || self.rootfs_services_are_ready().await
    }

    fn rootfs_services_required(&self) -> bool {
        rootfs_services_required_for(
            self.rootfs_entrypoint_expected.load(Ordering::SeqCst),
            ["", MERGE_TARGET],
        )
    }

    async fn rootfs_services_are_ready(&self) -> bool {
        if self.rootfs_services_ready.load(Ordering::SeqCst) {
            return true;
        }
        if fs::metadata(ROOTFS_SERVICES_READY_PATH).await.is_ok() {
            self.mark_rootfs_services_ready();
            return true;
        }
        false
    }
}

fn rootfs_services_required_for<'a>(
    entrypoint_expected: bool,
    feature_roots: impl IntoIterator<Item = &'a str>,
) -> bool {
    entrypoint_expected && feature_enabled_in_roots("envd", feature_roots)
}

fn feature_enabled_in_roots<'a>(name: &str, roots: impl IntoIterator<Item = &'a str>) -> bool {
    roots.into_iter().any(|root| {
        let base = if root.is_empty() { "/" } else { root };
        let path = PathBuf::from(base)
            .join("etc")
            .join("conch")
            .join("features")
            .join(name);
        path.exists()
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::io;
    use std::path::PathBuf;

    #[test]
    fn rootfs_services_required_matches_go_envd_gate() {
        let guest = temp_dir("guest-feature").expect("guest temp dir");
        let merge = temp_dir("merge-feature").expect("merge temp dir");
        fs::create_dir_all(merge.join("etc/conch/features")).expect("feature dir");
        fs::write(merge.join("etc/conch/features/envd"), "").expect("feature file");

        let guest_root = guest.to_string_lossy().to_string();
        let merge_root = merge.to_string_lossy().to_string();
        let guest_root = guest_root.as_str();
        let merge_root = merge_root.as_str();

        assert!(!rootfs_services_required_for(
            true,
            [guest_root, guest_root]
        ));
        assert!(!rootfs_services_required_for(
            false,
            [guest_root, merge_root]
        ));
        assert!(rootfs_services_required_for(true, [guest_root, merge_root]));
        assert!(feature_enabled_in_roots("envd", [guest_root, merge_root]));
        assert!(!feature_enabled_in_roots(
            "missing",
            [guest_root, merge_root]
        ));
    }

    fn temp_dir(name: &str) -> io::Result<PathBuf> {
        let dir = std::env::temp_dir().join(format!(
            "conch-init-state-test-{name}-{}-{}",
            std::process::id(),
            unique_suffix()
        ));
        fs::create_dir_all(&dir)?;
        Ok(dir)
    }

    fn unique_suffix() -> u128 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .expect("time")
            .as_nanos()
    }
}
