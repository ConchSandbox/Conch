use std::io;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use tokio::fs;
use tokio::process::Command;
use tracing::{error, info, warn};

use crate::constants::MERGE_TARGET;
use crate::util::{cstr, is_mount_point, resolve_command};

pub async fn ensure_proc_mounted() {
    if is_mount_point("/proc") {
        return;
    }
    let _ = fs::create_dir_all("/proc").await;
    if let Err(err) = mount_fs(
        "none",
        "/proc",
        "proc",
        0,
        "",
        &["-t", "proc", "none", "/proc"],
    )
    .await
    {
        warn!(error = %err, "Failed to bootstrap mount /proc");
    }
}

pub async fn create_dev_null() {
    let _ = fs::create_dir_all("/dev").await;
    let dev = libc::makedev(1, 3);
    let ret = unsafe { libc::mknod(cstr("/dev/null").as_ptr(), libc::S_IFCHR | 0o666, dev) };
    if ret != 0 {
        warn!(error = %io::Error::last_os_error(), "Failed to create /dev/null");
    }
}

pub async fn mount_essential_filesystems() {
    for (fstype, target) in [
        ("proc", "/proc"),
        ("sysfs", "/sys"),
        ("tmpfs", "/tmp"),
        ("devtmpfs", "/dev"),
    ] {
        let _ = fs::create_dir_all(target).await;
        match mount_fs(
            "none",
            target,
            fstype,
            0,
            "",
            &["-t", fstype, "none", target],
        )
        .await
        {
            Ok(()) => info!(fstype, target, "Mounted filesystem"),
            Err(err) => error!(fstype, target, error = %err, "Failed to mount filesystem"),
        }
    }
}

pub async fn mount_storage_devices() {
    if fs::metadata("/dev/vda").await.is_err() {
        let dev = libc::makedev(253, 0);
        let ret = unsafe { libc::mknod(cstr("/dev/vda").as_ptr(), libc::S_IFBLK | 0o600, dev) };
        if ret != 0 {
            warn!(error = %io::Error::last_os_error(), "Failed to create /dev/vda");
        }
    }

    let mut upper_dir = PathBuf::from("/mnt/conch/upper");
    let mut work_dir = PathBuf::from("/mnt/conch/work");
    let _ = fs::create_dir_all("/mnt/disk").await;
    if mount_fs(
        "/dev/vda",
        "/mnt/disk",
        "ext4",
        0,
        "",
        &["-t", "ext4", "/dev/vda", "/mnt/disk"],
    )
    .await
    .is_ok()
    {
        info!(device = "/dev/vda", "Persistent disk mounted");
        upper_dir = PathBuf::from("/mnt/disk/upper");
        work_dir = PathBuf::from("/mnt/disk/work");
    } else {
        info!("Using RAM for writable layer");
    }
    let _ = fs::create_dir_all(&upper_dir).await;
    let _ = fs::create_dir_all(&work_dir).await;
    let _ = fs::create_dir_all(MERGE_TARGET).await;

    let lower_dirs = mount_pmem_devices().await;
    if !lower_dirs.is_empty() {
        mount_overlay_fs(&lower_dirs, &upper_dir, &work_dir).await;
    }
}

async fn mount_pmem_devices() -> String {
    let deadline = Instant::now() + Duration::from_millis(500);
    let entries = loop {
        let entries = glob_dev_pmem().await;
        if !entries.is_empty() || Instant::now() > deadline {
            break entries;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    };
    if entries.is_empty() {
        warn!("No pmem devices found");
        return String::new();
    }
    let mut lower_dirs = Vec::new();
    for device in entries {
        let Some(name) = device.file_name().and_then(|n| n.to_str()) else {
            continue;
        };
        let mount_point = format!("/mnt/conch/{name}");
        let _ = fs::create_dir_all(&mount_point).await;
        let device_s = device.to_string_lossy().to_string();
        if let Err(err) = mount_fs(
            &device_s,
            &mount_point,
            "erofs",
            libc::MS_RDONLY as usize,
            "",
            &["-t", "erofs", "-o", "ro", &device_s, &mount_point],
        )
        .await
        {
            error!(device = %device.display(), target = %mount_point, error = %err, "Failed to mount pmem device");
            continue;
        }
        prepend_lower_dir(&mut lower_dirs, mount_point);
    }
    lower_dirs.join(":")
}

fn prepend_lower_dir(lower_dirs: &mut Vec<String>, mount_point: String) {
    lower_dirs.insert(0, mount_point);
}

async fn glob_dev_pmem() -> Vec<PathBuf> {
    let mut out = Vec::new();
    if let Ok(mut dir) = fs::read_dir("/dev").await {
        while let Ok(Some(entry)) = dir.next_entry().await {
            if entry.file_name().to_string_lossy().starts_with("pmem") {
                out.push(entry.path());
            }
        }
    }
    out.sort();
    out
}

async fn mount_overlay_fs(lower_dirs: &str, upper_dir: &Path, work_dir: &Path) {
    let opts = format!(
        "lowerdir={lower_dirs},upperdir={},workdir={}",
        upper_dir.display(),
        work_dir.display()
    );
    if let Err(err) = mount_fs(
        "overlay",
        MERGE_TARGET,
        "overlay",
        0,
        &opts,
        &["-t", "overlay", "overlay", "-o", &opts, MERGE_TARGET],
    )
    .await
    {
        error!(target = MERGE_TARGET, error = %err, "Failed to mount OverlayFS");
    } else {
        info!(target = MERGE_TARGET, "Mounted OverlayFS");
    }
}

pub async fn prepare_merge_root() {
    let _ = fs::create_dir_all(format!("{MERGE_TARGET}/root")).await;
    let passwd_file = format!("{MERGE_TARGET}/etc/passwd");
    if let Ok(content) = fs::read_to_string(&passwd_file).await {
        let mut out = Vec::new();
        for line in content.lines() {
            if line.starts_with("root:") {
                let mut parts = line.split(':').map(ToString::to_string).collect::<Vec<_>>();
                if parts.len() >= 7 {
                    parts[5] = "/root".into();
                    out.push(parts[..7].join(":"));
                    continue;
                }
            }
            out.push(line.to_string());
        }
        let _ = fs::write(&passwd_file, format!("{}\n", out.join("\n"))).await;
        info!("Ensured /etc/passwd root home is /root");
    }
}

pub async fn bind_mount_to_merge() {
    for dir in ["/proc", "/sys", "/dev", "/tmp"] {
        let target = format!("{MERGE_TARGET}{dir}");
        let _ = fs::create_dir_all(&target).await;
        if let Err(err) = mount_fs(
            dir,
            &target,
            "",
            libc::MS_BIND as usize,
            "",
            &["--bind", dir, &target],
        )
        .await
        {
            error!(source = dir, target = %target, error = %err, "Failed to bind mount");
        }
    }
}

pub async fn mount_fs(
    source: &str,
    target: &str,
    fstype: &str,
    flags: usize,
    data: &str,
    cmd_args: &[&str],
) -> io::Result<()> {
    if !cmd_args.is_empty() {
        if let Some(mount_path) = resolve_command(
            "mount",
            &[
                "/bin/mount",
                "/usr/bin/mount",
                "/sbin/mount",
                "/usr/sbin/mount",
            ],
        ) {
            if Command::new(mount_path)
                .args(cmd_args)
                .status()
                .await
                .map(|s| s.success())
                .unwrap_or(false)
            {
                return Ok(());
            }
        }
    }
    let source_c = cstr(source);
    let target_c = cstr(target);
    let fstype_c = if fstype.is_empty() {
        None
    } else {
        Some(cstr(fstype))
    };
    let data_c = if data.is_empty() {
        None
    } else {
        Some(cstr(data))
    };
    let fstype_ptr = fstype_c
        .as_ref()
        .map(|value| value.as_ptr())
        .unwrap_or(std::ptr::null());
    let data_ptr = data_c
        .as_ref()
        .map(|value| value.as_ptr().cast())
        .unwrap_or(std::ptr::null());
    let ret = unsafe {
        libc::mount(
            source_c.as_ptr(),
            target_c.as_ptr(),
            fstype_ptr,
            flags as libc::c_ulong,
            data_ptr,
        )
    };
    if ret == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

#[cfg(test)]
mod tests {
    use super::prepend_lower_dir;

    #[test]
    fn lower_dirs_preserve_go_overlay_order_for_sorted_pmem_devices() {
        let mut lower_dirs = Vec::new();
        for mount_point in ["/mnt/conch/pmem0", "/mnt/conch/pmem1", "/mnt/conch/pmem2"] {
            prepend_lower_dir(&mut lower_dirs, mount_point.to_string());
        }

        assert_eq!(
            lower_dirs.join(":"),
            "/mnt/conch/pmem2:/mnt/conch/pmem1:/mnt/conch/pmem0"
        );
    }
}
