#!/usr/bin/env bash
set -euo pipefail

BUILDAH_CMD="${CONCH_BUILDAH_BIN:-buildah}"
IMAGE_REPO="localhost/conch/kernel"
VERSION="latest"
X86_DIR=""
ARM_DIR=""
BUILD_DIR="$(pwd)"
KERNEL_FILE=""
INITRD_FILE=""
ARCH="$(uname -m)"
PUSH="false"
PLAIN_HTTP="false"
CREDS=""
DRY_RUN="false"

usage() {
    cat <<'EOF'
Usage:
  bash scripts/build-kernel-image.sh [options]

Description:
  Build Conch kernel images from bzImage and conch.initrd.

  The preferred release flow is to build both x86_64 and aarch64 images, then
  publish them under one multi-arch tag such as:

    hub.oepkgs.net/conch/kernel:6.6.0

Options:
  --x86-dir DIR          Directory containing x86_64 bzImage and conch.initrd
  --arm-dir DIR          Directory containing aarch64 bzImage and conch.initrd
  --build-dir DIR        Single-arch input directory
                         default: current working directory
  --kernel FILE          Single-arch kernel file path
                         default: <build-dir>/bzImage
  --initrd FILE          Single-arch initrd file path
                         default: <build-dir>/conch.initrd
  --arch ARCH            Single-arch target architecture
                         default: uname -m
  --repo REPO            Target image repository
                         default: localhost/conch/kernel
  --version VERSION      Kernel version used in the image tag
                         default: latest
  --push                 Push the final image / manifest to docker://<repo>:<version>
  --plain-http           Push with --tls-verify=false
  --user USER:PASS       Registry credentials for push
  --dry-run              Print actions without executing buildah
  -h, --help             Show this help message

Examples:
  # Build a single-arch local kernel image.
  bash scripts/build-kernel-image.sh --build-dir ./kernel-x86 --arch x86_64 --repo hub.oepkgs.net/conch/kernel --version 6.6.0

  # Build and push a multi-arch kernel image from two directories.
  bash scripts/build-kernel-image.sh \
    --x86-dir ./kernel-x86 \
    --arm-dir ./kernel-arm \
    --repo hub.oepkgs.net/conch/kernel \
    --version 6.6.0 \
    --push
EOF
}

log() {
    printf '[INFO] %s\n' "$*"
}

die() {
    printf '[ERROR] %s\n' "$*" >&2
    exit 1
}

run_cmd() {
    if [[ "${DRY_RUN}" == "true" ]]; then
        printf '[DRY-RUN] %q' "$1"
        shift
        for arg in "$@"; do
            printf ' %q' "$arg"
        done
        printf '\n'
        return 0
    fi
    "$@"
}

normalize_arch() {
    case "$1" in
        x86_64|amd64)
            printf 'amd64\n'
            ;;
        aarch64|arm64)
            printf 'arm64\n'
            ;;
        *)
            die "unsupported architecture: $1"
            ;;
    esac
}

platform_for_arch() {
    local arch
    arch="$(normalize_arch "$1")"
    printf 'linux/%s\n' "${arch}"
}

final_tag() {
    printf '%s:%s\n' "${IMAGE_REPO}" "${VERSION}"
}

local_arch_tag() {
    local arch
    arch="$(normalize_arch "$1")"
    printf '%s:%s-%s\n' "${IMAGE_REPO}" "${VERSION}" "${arch}"
}

push_args() {
    if [[ "${PLAIN_HTTP}" == "true" ]]; then
        printf '%s\n' "--tls-verify=false"
    fi
    if [[ -n "${CREDS}" ]]; then
        printf '%s\n' "--creds"
        printf '%s\n' "${CREDS}"
    fi
}

build_kernel_image() {
    local arch="$1"
    local input_dir="$2"
    local image_tag="$3"
    local kernel_file="${input_dir}/bzImage"
    local initrd_file="${input_dir}/conch.initrd"
    local cid=""

    [[ -f "${kernel_file}" ]] || die "kernel file not found: ${kernel_file}"
    [[ -f "${initrd_file}" ]] || die "initrd file not found: ${initrd_file}"

    log "Building kernel image: ${image_tag} (${arch})"
    log "Kernel file: ${kernel_file}"
    log "Initrd file: ${initrd_file}"

    if [[ "${DRY_RUN}" == "true" ]]; then
        run_cmd "${BUILDAH_CMD}" from scratch
        run_cmd "${BUILDAH_CMD}" copy "<cid>" "${kernel_file}" /boot/vmlinuz
        run_cmd "${BUILDAH_CMD}" copy "<cid>" "${initrd_file}" /data/conch.initrd
        run_cmd "${BUILDAH_CMD}" config --arch "$(normalize_arch "${arch}")" --label io.conch.type=combined --label io.conch.kernel=bzImage --label io.conch.initrd=present "<cid>"
        run_cmd "${BUILDAH_CMD}" commit "<cid>" "${image_tag}"
        return 0
    fi

    cid="$("${BUILDAH_CMD}" from scratch)"
    "${BUILDAH_CMD}" copy "${cid}" "${kernel_file}" /boot/vmlinuz
    "${BUILDAH_CMD}" copy "${cid}" "${initrd_file}" /data/conch.initrd
    "${BUILDAH_CMD}" config \
        --arch "$(normalize_arch "${arch}")" \
        --label io.conch.type=combined \
        --label io.conch.kernel=bzImage \
        --label io.conch.initrd=present \
        "${cid}"
    "${BUILDAH_CMD}" commit "${cid}" "${image_tag}"
    "${BUILDAH_CMD}" rm "${cid}" >/dev/null 2>&1 || true
}

push_image() {
    local image_tag="$1"
    local args=()
    while IFS= read -r arg; do
        args+=("${arg}")
    done < <(push_args)

    run_cmd "${BUILDAH_CMD}" push "${args[@]}" "${image_tag}" "docker://${image_tag}"
}

create_multiarch_manifest() {
    local manifest_tag="$1"
    local x86_tag="$2"
    local arm_tag="$3"
    local args=()

    log "Creating multi-arch manifest: ${manifest_tag}"
    if [[ "${DRY_RUN}" == "true" ]]; then
        run_cmd "${BUILDAH_CMD}" manifest rm "${manifest_tag}"
    else
        "${BUILDAH_CMD}" manifest rm "${manifest_tag}" >/dev/null 2>&1 || true
    fi
    run_cmd "${BUILDAH_CMD}" manifest create "${manifest_tag}"
    run_cmd "${BUILDAH_CMD}" manifest add --arch amd64 --os linux "${manifest_tag}" "${x86_tag}"
    run_cmd "${BUILDAH_CMD}" manifest add --arch arm64 --os linux "${manifest_tag}" "${arm_tag}"

    if [[ "${PUSH}" == "true" ]]; then
        while IFS= read -r arg; do
            args+=("${arg}")
        done < <(push_args)
        run_cmd "${BUILDAH_CMD}" manifest push --all "${args[@]}" "${manifest_tag}" "docker://${manifest_tag}"
        log "Pushed multi-arch kernel image: ${manifest_tag}"
    else
        log "Multi-arch kernel image built locally: ${manifest_tag}"
        log "Push example: buildah manifest push --all ${manifest_tag} docker://${manifest_tag}"
    fi
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --x86-dir)
            X86_DIR="$2"
            shift 2
            ;;
        --arm-dir)
            ARM_DIR="$2"
            shift 2
            ;;
        --build-dir)
            BUILD_DIR="$2"
            shift 2
            ;;
        --kernel)
            KERNEL_FILE="$2"
            shift 2
            ;;
        --initrd)
            INITRD_FILE="$2"
            shift 2
            ;;
        --arch)
            ARCH="$2"
            shift 2
            ;;
        --repo)
            IMAGE_REPO="$2"
            shift 2
            ;;
        --version)
            VERSION="$2"
            shift 2
            ;;
        --push)
            PUSH="true"
            shift
            ;;
        --plain-http)
            PLAIN_HTTP="true"
            shift
            ;;
        --user)
            CREDS="$2"
            shift 2
            ;;
        --dry-run)
            DRY_RUN="true"
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            die "unknown option: $1"
            ;;
    esac
done

[[ -x "$(command -v "${BUILDAH_CMD}")" ]] || die "buildah not found: ${BUILDAH_CMD}"

if [[ -n "${X86_DIR}" || -n "${ARM_DIR}" ]]; then
    [[ -n "${X86_DIR}" ]] || die "--x86-dir is required for multi-arch build"
    [[ -n "${ARM_DIR}" ]] || die "--arm-dir is required for multi-arch build"

    X86_TAG="$(local_arch_tag amd64)"
    ARM_TAG="$(local_arch_tag arm64)"
    MANIFEST_TAG="$(final_tag)"

    build_kernel_image amd64 "${X86_DIR}" "${X86_TAG}"
    build_kernel_image arm64 "${ARM_DIR}" "${ARM_TAG}"
    create_multiarch_manifest "${MANIFEST_TAG}" "${X86_TAG}" "${ARM_TAG}"
    exit 0
fi

if [[ -n "${KERNEL_FILE}" || -n "${INITRD_FILE}" ]]; then
    [[ -n "${KERNEL_FILE}" ]] || KERNEL_FILE="${BUILD_DIR}/bzImage"
    [[ -n "${INITRD_FILE}" ]] || INITRD_FILE="${BUILD_DIR}/conch.initrd"
    tmp_dir="$(mktemp -d)"
    trap 'rm -rf "${tmp_dir}"' EXIT
    ln -s "$(realpath "${KERNEL_FILE}")" "${tmp_dir}/bzImage"
    ln -s "$(realpath "${INITRD_FILE}")" "${tmp_dir}/conch.initrd"
    BUILD_DIR="${tmp_dir}"
fi

IMAGE_TAG="$(local_arch_tag "${ARCH}")"
build_kernel_image "${ARCH}" "${BUILD_DIR}" "${IMAGE_TAG}"

if [[ "${PUSH}" == "true" ]]; then
    push_image "${IMAGE_TAG}"
else
    log "Kernel image built successfully: ${IMAGE_TAG}"
    log "Push example: buildah push ${IMAGE_TAG} docker://${IMAGE_TAG}"
fi
