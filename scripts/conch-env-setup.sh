#!/bin/bash
set -euo pipefail

###############################################################################
# Script Name: conch-env-setup.sh
# Description: Automates Conch environment setup with one-click execution
# Core Features:
#   1. Check and install runtime dependencies (cloud-hypervisor, erofs-utils)
#   2. Build Conch binaries locally
#   3. Pull function images through conchd
#
# MUST run in Conch project directory:
#   From Conch root: ./scripts/conch-env-setup.sh
#
# Usage:
#   install: Installs cloud-hypervisor, erofs-utils, builds Conch, and installs SDK.
#   pull: Pulls the required image through a running conchd.
#   build: Builds Conch binaries locally.
#   all: Executes all setup steps: provisioning, local build, and SDK setup.
#   Customization: Use --main_image to switch the default image on the fly.
###############################################################################

###############################################################################
# Architecture Detection and Image Selection
# - x86_64  -> -x86 suffix for images, cloud-hypervisor-static
# - aarch64 -> -aarch suffix for images, cloud-hypervisor-static-aarch64
###############################################################################
ARCH=$(uname -m)
case $ARCH in
    x86_64)
        ARCH_SUFFIX="x86"
        CLH_BINARY="cloud-hypervisor-static"
        ;;
    aarch64)
        ARCH_SUFFIX="aarch"
        CLH_BINARY="cloud-hypervisor-static-aarch64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"; exit 1
        ;;
esac

F_IMG_DEFAULT="hub.oepkgs.net/conch/openeuler:odd-${ARCH_SUFFIX}"

# Cloud-Hypervisor version and download URL
CLH_VER="52.0-conch"
CLH_URL="https://github.com/ConchSandbox/cloud-hypervisor/releases/download/v${CLH_VER}/${CLH_BINARY}"

show_help() {
    echo "Usage: $0 [COMMAND] [OPTIONS]"
    echo ""
    echo "Commands:"
    echo "  provisioning           Install cloud-hypervisor and erofs-utils"
    echo "  pull                   Pull function image and run unpack"
    echo "  build                  Build Conch binaries locally"
    echo "  sdk                    Install Python SDK in editable mode"
    echo "  install                Quick setup (provisioning + build + sdk)"
    echo "  all                    Run all setup steps (provisioning + build + sdk)"
    echo "  help                   Show this help message"
    echo ""
    echo "Options:"
    echo "  --main_image=VALUE     Specify the main/function image (default: $F_IMG_DEFAULT)"
}

MAIN_IMG=$F_IMG_DEFAULT
COMMAND=${1:-help}
if [ "$#" -gt 0 ]; then
    shift
fi

for i in "$@"; do
    case $i in
        --main_image=*)  MAIN_IMG="${i#*=}" ;;
    esac
done

install_clh() {
    echo "--- Checking Cloud-Hypervisor ---"
    CLH_MIN_VER=51
    CLH_NEED_INSTALL=0
    CLH_BIN_PATH=""

    # Check if cloud-hypervisor exists in PATH and is a valid executable
    CLH_BIN_PATH=$(command -v cloud-hypervisor 2>/dev/null || true)
    if [ -n "$CLH_BIN_PATH" ] && [ -s "$CLH_BIN_PATH" ] && [ -x "$CLH_BIN_PATH" ]; then
        # File exists, is non-empty, and is executable - verify it actually works
        CLH_VER_STR=$($CLH_BIN_PATH --version 2>&1 | awk '{print $2}' | sed 's/v//')
        CLH_MAJOR=$(echo "$CLH_VER_STR" | cut -d'.' -f1)
        if [ -z "$CLH_MAJOR" ] || [ "$CLH_MAJOR" -lt "$CLH_MIN_VER" ] 2>/dev/null; then
            echo "cloud-hypervisor version v${CLH_VER_STR:-unknown} is below the required v${CLH_MIN_VER}.0, reinstalling..."
            CLH_NEED_INSTALL=1
        else
            echo "cloud-hypervisor v${CLH_VER_STR} already installed and meets the minimum version requirement (>= v${CLH_MIN_VER}.0)."
        fi
    else
        # command not found, or file is empty/invalid
        if [ -f "$CLH_BIN_PATH" ]; then
            echo "cloud-hypervisor exists but is invalid (empty or not executable), reinstalling..."
        else
            echo "cloud-hypervisor not found, installing..."
        fi
        CLH_NEED_INSTALL=1
    fi

    if [ "$CLH_NEED_INSTALL" -eq 1 ]; then
        # Remove potentially invalid binary first
        rm -f /usr/local/bin/cloud-hypervisor

        echo "Downloading cloud-hypervisor v${CLH_VER} for ${ARCH}..."
        echo "URL: $CLH_URL"
        if ! wget --progress=bar:force "$CLH_URL" -O /usr/local/bin/cloud-hypervisor 2>&1; then
            echo "Error: Failed to download cloud-hypervisor."
            echo "Manual download: https://github.com/ConchSandbox/cloud-hypervisor/releases"
            return 1
        fi
        chmod +x /usr/local/bin/cloud-hypervisor
        echo "cloud-hypervisor v${CLH_VER} installed successfully for ${ARCH}."
    fi
    echo "Cloud-Hypervisor check completed successfully."
}

install_erofs() {
    echo "--- Installing EROFS Utils ---"
    if command -v dnf >/dev/null 2>&1; then
        sudo dnf install -y erofs-utils
    elif command -v yum >/dev/null 2>&1; then
        sudo yum install -y erofs-utils
    elif command -v apt-get >/dev/null 2>&1; then
        sudo apt-get update
        sudo apt-get install -y erofs-utils
    else
        echo "unsupported package manager; please install erofs-utils manually" >&2
        exit 1
    fi

    if ! command -v mkfs.erofs >/dev/null 2>&1; then
        echo "missing required command: mkfs.erofs" >&2
        exit 1
    fi
    if ! mkfs.erofs --help 2>&1 | grep -q -- "--fsalignblks"; then
        echo "mkfs.erofs does not support --fsalignblks; please install erofs-utils 1.9 or newer." >&2
        exit 1
    fi
    echo "erofs-utils installed and verified successfully."
}

pull_function() {
    echo "--- Pulling Function Image via conch ---"
    echo "Note: conchd must be running before pull; conch talks to conchd over the configured API endpoint."
    if [ -x "./bin/conch" ]; then
        ./bin/conch image pull "$MAIN_IMG"
        echo "Function image pull completed successfully."
    else
        echo "Error: ./bin/conch executable not found."
        return 1
    fi
}

run_build() {
    echo "--- Building Conch binaries locally ---"
    make gen-proto
    mkdir -p bin
    version_tag=$(git describe --tags --abbrev=0 2>/dev/null || echo unknown)
    git_commit=$(git rev-parse --short=8 HEAD 2>/dev/null || echo unknown)
    version_pkg="github.com/openeuler/Conch/internal/version"
    for dir in cmd/*; do
        [ -d "$dir" ] || continue
        name=$(basename "$dir")
        [ "$name" = "conch-unpack" ] && continue
        echo "building $name..."
        go build -ldflags "-X ${version_pkg}.Version=${version_tag} -X ${version_pkg}.Commit=${git_commit}" -o "bin/$name" "./cmd/$name"
    done
    echo "Conch build completed successfully."
}

install_sdk() {
    echo "--- Installing Python SDK ---"
    if [ -d "./sdk" ]; then
        if ! pip install -e ./sdk --break-system-packages  --ignore-installed typing-extensions; then
            echo "Error: Failed to install SDK with pip."
            return 1
        fi

        # Setup config
        [ ! -d "/etc/conch" ] && mkdir -p /etc/conch
        if [ ! -f "/etc/conch/sdk-config.yaml" ] && [ -f "./config/sdk-config.yaml" ]; then
            cp ./config/sdk-config.yaml /etc/conch/sdk-config.yaml
            echo "Config file copied to /etc/conch/sdk-config.yaml"
        else
            echo "Skipping config copy (/etc/conch/sdk-config.yaml already exists or source missing)"
        fi
        echo "Python SDK install completed successfully."
    else
        echo "Error: ./sdk directory not found."
        return 1
    fi
}

case "$COMMAND" in
    provisioning) install_clh && install_erofs ;;
    pull)    pull_function ;;
    build)   run_build ;;
    sdk)     install_sdk ;;
    install) install_clh && install_erofs && run_build && install_sdk;;
    all)     install_clh && install_erofs && run_build && install_sdk;;
    help|*)  show_help ;;
esac
