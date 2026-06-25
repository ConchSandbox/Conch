use tokio::fs;
use tracing::warn;

use crate::constants::MERGE_TARGET;
use crate::mount::mount_fs;

pub async fn setup_dev_pts() {
    let target = format!("{MERGE_TARGET}/dev/pts");
    let _ = fs::create_dir_all(&target).await;
    if let Err(err) = mount_fs(
        "devpts",
        &target,
        "devpts",
        0,
        "newinstance,ptmxmode=0666,mode=0620",
        &["-t", "devpts", "devpts", &target],
    )
    .await
    {
        warn!(target = %target, error = %err, "Failed to mount devpts");
    }
}
