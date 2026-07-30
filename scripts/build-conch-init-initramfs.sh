#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

INIT_BIN="${INIT_BIN:-${REPO_ROOT}/bin/conch-init}"
OUTPUT="${OUTPUT:-${REPO_ROOT}/build-artifacts/conch-init-initramfs.cpio.gz}"
WORK_DIR="${WORK_DIR:-}"
ROOTFS_DIR="${ROOTFS_DIR:-}"
MODULES_DIR="${MODULES_DIR:-}"
KEEP_ROOTFS=0

usage() {
    cat <<EOF
Usage: $0 [options]

Build a minimal initramfs that runs static conch-init as PID 1.

Options:
  --init-bin PATH        conch-init binary to install into /sbin/conch-init
                         (default: ${INIT_BIN})
  --output PATH          output initramfs path
                         (default: ${OUTPUT})
  --work-dir DIR         working directory for downloads and temporary files
  --rootfs-dir DIR       rootfs directory to create before packing
                         (default: <work-dir>/rootfs)
  --modules-dir DIR      optional kernel modules directory copied to /lib/modules
  --keep-rootfs          keep the generated rootfs directory
  -h, --help             show this help message
EOF
}

die() {
    echo "error: $*" >&2
    exit 1
}

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

warn() {
    echo "warning: $*" >&2
}

create_chrdev() {
    local path="$1"
    local mode="$2"
    local major="$3"
    local minor="$4"

    if ! command -v mknod >/dev/null 2>&1; then
        warn "mknod not found; ${path} will be created by conch-init at boot"
        return 0
    fi

    if [ -e "$ROOTFS_DIR$path" ]; then
        return 0
    fi

    if mknod "$ROOTFS_DIR$path" c "$major" "$minor" 2>/dev/null; then
        chmod "$mode" "$ROOTFS_DIR$path"
        return 0
    fi

    warn "failed to create ${path}; run as root to embed device nodes, or rely on conch-init boot-time creation"
    return 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --init-bin)
            [ $# -ge 2 ] || die "missing value for $1"
            INIT_BIN="$2"
            shift 2
            ;;
        --output)
            [ $# -ge 2 ] || die "missing value for $1"
            OUTPUT="$2"
            shift 2
            ;;
        --work-dir)
            [ $# -ge 2 ] || die "missing value for $1"
            WORK_DIR="$2"
            shift 2
            ;;
        --rootfs-dir)
            [ $# -ge 2 ] || die "missing value for $1"
            ROOTFS_DIR="$2"
            shift 2
            ;;
        --modules-dir)
            [ $# -ge 2 ] || die "missing value for $1"
            MODULES_DIR="$2"
            shift 2
            ;;
        --keep-rootfs)
            KEEP_ROOTFS=1
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            die "unknown argument: $1"
            ;;
    esac
done

for tool in gzip cpio find install mkdir ln rm cp ls mktemp dirname chmod; do
    require_cmd "$tool"
done

[ -f "$INIT_BIN" ] || die "conch-init binary does not exist: $INIT_BIN"
[ -z "$MODULES_DIR" ] || [ -d "$MODULES_DIR" ] || die "kernel modules directory does not exist: $MODULES_DIR"

created_work_dir=0
if [ -z "$WORK_DIR" ]; then
    WORK_DIR="$(mktemp -d)"
    created_work_dir=1
fi

if [ -z "$ROOTFS_DIR" ]; then
    ROOTFS_DIR="${WORK_DIR}/rootfs"
fi

case "$ROOTFS_DIR" in
    ""|"/") die "refusing to use unsafe rootfs directory: $ROOTFS_DIR" ;;
esac

cleanup() {
    if [ "$created_work_dir" -eq 1 ] && [ "$KEEP_ROOTFS" -eq 0 ]; then
        rm -rf "$WORK_DIR"
    fi
}
trap cleanup EXIT

mkdir -p "$WORK_DIR" "$(dirname "$OUTPUT")"
rm -rf "$ROOTFS_DIR"
mkdir -p "$ROOTFS_DIR"

mkdir -p \
    "$ROOTFS_DIR"/{bin,sbin,etc,proc,sys,dev/pts,run,tmp,var/log/conch-init,mnt/disk,mnt/conch/upper,mnt/conch/work,mnt/conch/merge,lib/modules}
chmod 0755 \
    "$ROOTFS_DIR" \
    "$ROOTFS_DIR"/{bin,sbin,etc,proc,sys,dev,dev/pts,run,var,var/log,var/log/conch-init,mnt,mnt/disk,mnt/conch,mnt/conch/upper,mnt/conch/work,mnt/conch/merge,lib,lib/modules}
chmod 1777 "$ROOTFS_DIR/tmp"

install -m 0755 "$INIT_BIN" "$ROOTFS_DIR/sbin/conch-init"
ln -sf sbin/conch-init "$ROOTFS_DIR/init"

create_chrdev /dev/null 0666 1 3
create_chrdev /dev/zero 0666 1 5
create_chrdev /dev/full 0666 1 7
create_chrdev /dev/random 0666 1 8
create_chrdev /dev/urandom 0666 1 9
create_chrdev /dev/tty 0666 5 0
create_chrdev /dev/console 0600 5 1

if [ -n "$MODULES_DIR" ]; then
    cp -a "$MODULES_DIR"/. "$ROOTFS_DIR/lib/modules"/
fi

(
    cd "$ROOTFS_DIR"
    find . -print0 | cpio --null -o --format=newc | gzip -9
) > "$OUTPUT"

ls -lh "$OUTPUT"
