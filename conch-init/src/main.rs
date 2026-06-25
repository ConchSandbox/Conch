mod auth;
mod constants;
mod devpts;
mod grpc;
mod init;
mod logging;
mod mount;
mod network;
mod pb;
mod reaper;
mod rootfs_entrypoint;
mod state;
mod util;
mod vsock;

use init::run_as_init;
use state::AgentState;

#[tokio::main]
async fn main() {
    logging::init();
    if let Err(err) = run().await {
        eprintln!("conch-init failed: {err}");
        std::process::exit(1);
    }
}

async fn run() -> Result<(), Box<dyn std::error::Error>> {
    for arg in std::env::args().skip(1) {
        if !matches!(arg.as_str(), "-init" | "--init") {
            return Err(format!("unknown argument: {arg}").into());
        }
    }

    let state = AgentState::new();
    run_as_init(state).await?;
    Ok(())
}
