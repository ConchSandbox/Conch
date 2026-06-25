use std::env;
use std::ffi::CString;
use std::io;
use std::path::{Path, PathBuf};

use tokio::process::Command;
use tracing::warn;

pub fn get_sandbox_id_from_cmdline() -> Option<String> {
    std::fs::read_to_string("/proc/cmdline")
        .ok()
        .and_then(|data| {
            data.split_whitespace()
                .find_map(|field| {
                    field
                        .strip_prefix("conch.sandbox_id=")
                        .map(ToString::to_string)
                })
                .filter(|v| !v.is_empty())
        })
}

pub fn chroot_to(path: &str) -> io::Result<()> {
    let c_path = CString::new(path)?;
    let slash = CString::new("/")?;
    if unsafe { libc::chroot(c_path.as_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::chdir(slash.as_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

pub fn clean_path_string(path: &str) -> String {
    let mut components = Vec::new();
    let absolute = path.starts_with('/');
    for part in path.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                components.pop();
            }
            v => components.push(v),
        }
    }
    let mut out = if absolute {
        "/".to_string()
    } else {
        String::new()
    };
    out.push_str(&components.join("/"));
    if out.is_empty() { ".".into() } else { out }
}

pub fn clean_agent_filepath(path: &str, operation: &str) -> Result<PathBuf, String> {
    if path.is_empty() {
        return Err(format!("filepath is required for {operation}"));
    }
    let cleaned = PathBuf::from(clean_path_string(path));
    if cleaned == PathBuf::from(".") {
        return Err(format!("invalid filepath for {operation}"));
    }
    Ok(cleaned)
}

pub fn resolve_command(name: &str, candidates: &[&str]) -> Option<String> {
    for candidate in candidates {
        if Path::new(candidate).is_file() {
            return Some((*candidate).to_string());
        }
    }
    env::var_os("PATH").and_then(|path| {
        env::split_paths(&path)
            .map(|dir| dir.join(name))
            .find(|path| path.is_file())
            .map(|path| path.to_string_lossy().to_string())
    })
}

pub async fn run_command_warn(name: &str, args: &[&str], message: &str) {
    let cmd = resolve_command(
        name,
        &["/sbin/ip", "/usr/sbin/ip", "/usr/bin/ip", "/bin/ip"],
    )
    .unwrap_or_else(|| name.into());
    if let Err(err) = Command::new(cmd).args(args).status().await {
        warn!(error = %err, "{message}");
    }
}

pub fn is_mount_point(target: &str) -> bool {
    std::fs::read_to_string("/proc/self/mountinfo")
        .map(|data| {
            data.lines()
                .any(|line| line.split_whitespace().nth(4) == Some(target))
        })
        .unwrap_or(false)
}

pub fn cstr(value: &str) -> CString {
    CString::new(value).expect("path contains no interior nul")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clean_agent_filepath_rejects_empty_and_dot() {
        assert!(clean_agent_filepath("", "test").is_err());
        assert!(clean_agent_filepath(".", "test").is_err());
        assert_eq!(
            clean_agent_filepath("/tmp/../tmp/file", "test").unwrap(),
            PathBuf::from("/tmp/file")
        );
    }
}
