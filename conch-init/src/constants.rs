use std::time::Duration;

pub const AGENT_DESCRIPTOR_SET: &[u8] = tonic::include_file_descriptor_set!("agent_descriptor");

pub const SERVER_PORT: &str = "0.0.0.0:4064";
pub const SERVER_VERSION: &str = "0.0.4";
pub const VSOCK_READY_PORT: u32 = 4065;
pub const FILE_PERM: u32 = 0o644;
pub const AGENT_FILE_CHUNK_BYTES: usize = 1024 * 1024;
pub const AGENT_TOKEN_METADATA_KEY: &str = "x-conch-agent-token";
pub const MERGE_TARGET: &str = "/mnt/conch/merge";
pub const ROOTFS_SERVICES_READY_PATH: &str = "/run/conch/services-ready";
pub const SANDBOX_READY_RESPONSE_TIMEOUT: Duration = Duration::from_millis(1500);
