#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

ALPINE_VERSION="${ALPINE_VERSION:-3.20.3}"
ALPINE_MIRROR="${ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}"
ALPINE_ARCH="${ALPINE_ARCH:-}"
INIT_BIN="${INIT_BIN:-${REPO_ROOT}/bin/conch-init}"
OUTPUT="${OUTPUT:-${REPO_ROOT}/build-artifacts/conch-init-initramfs.cpio.gz}"
WORK_DIR="${WORK_DIR:-}"
ROOTFS_DIR="${ROOTFS_DIR:-}"
MODULES_DIR="${MODULES_DIR:-}"
KEEP_ROOTFS=0

usage() {
    cat <<EOF
Usage: $0 [options]

Build an Alpine initramfs that runs conch-init as PID 1.

Options:
  --init-bin PATH        conch-init binary to install into /sbin/conch-init
                         (default: ${INIT_BIN})
  --output PATH          output initramfs path
                         (default: ${OUTPUT})
  --work-dir DIR         working directory for downloads and temporary files
  --rootfs-dir DIR       rootfs directory to create before packing
                         (default: <work-dir>/rootfs)
  --modules-dir DIR      optional kernel modules directory copied to /lib/modules
  --alpine-version VER   Alpine minirootfs version
                         (default: ${ALPINE_VERSION})
  --alpine-mirror URL    Alpine mirror base URL
                         (default: ${ALPINE_MIRROR})
  --alpine-arch ARCH     Alpine architecture (default: detected from uname -m)
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

detect_alpine_arch() {
    case "$(uname -m)" in
        x86_64) echo "x86_64" ;;
        aarch64|arm64) echo "aarch64" ;;
        *) die "unsupported host architecture: $(uname -m)" ;;
    esac
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
        --alpine-version)
            [ $# -ge 2 ] || die "missing value for $1"
            ALPINE_VERSION="$2"
            shift 2
            ;;
        --alpine-mirror)
            [ $# -ge 2 ] || die "missing value for $1"
            ALPINE_MIRROR="${2%/}"
            shift 2
            ;;
        --alpine-arch)
            [ $# -ge 2 ] || die "missing value for $1"
            ALPINE_ARCH="$2"
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

for tool in curl tar gzip cpio find install mkdir ln rm cp ls mktemp dirname; do
    require_cmd "$tool"
done

[ -f "$INIT_BIN" ] || die "conch-init binary does not exist: $INIT_BIN"
[ -z "$MODULES_DIR" ] || [ -d "$MODULES_DIR" ] || die "kernel modules directory does not exist: $MODULES_DIR"

if [ -z "$ALPINE_ARCH" ]; then
    ALPINE_ARCH="$(detect_alpine_arch)"
fi
ALPINE_MIRROR="${ALPINE_MIRROR%/}"

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

alpine_series="v${ALPINE_VERSION%.*}"
alpine_tar="alpine-minirootfs-${ALPINE_VERSION}-${ALPINE_ARCH}.tar.gz"
alpine_url="${ALPINE_MIRROR}/${alpine_series}/releases/${ALPINE_ARCH}/${alpine_tar}"

mkdir -p "$WORK_DIR" "$(dirname "$OUTPUT")"
rm -rf "$ROOTFS_DIR"
mkdir -p "$ROOTFS_DIR"

echo "Downloading Alpine minirootfs: $alpine_url"
curl -fsSL "$alpine_url" -o "$WORK_DIR/$alpine_tar"
tar -xzf "$WORK_DIR/$alpine_tar" -C "$ROOTFS_DIR"

install -m 0755 "$INIT_BIN" "$ROOTFS_DIR/sbin/conch-init"
mkdir -p "$ROOTFS_DIR"/{proc,sys,dev,run,tmp,var/log/conch-init}

if [ -n "$MODULES_DIR" ]; then
    mkdir -p "$ROOTFS_DIR/lib/modules"
    cp -a "$MODULES_DIR"/. "$ROOTFS_DIR/lib/modules"/
fi

ln -sf sbin/conch-init "$ROOTFS_DIR/init"

(
    cd "$ROOTFS_DIR"
    find . -print0 | cpio --null -o --format=newc | gzip -9
) > "$OUTPUT"

ls -lh "$OUTPUT"
