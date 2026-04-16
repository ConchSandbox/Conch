#!/bin/bash

###############################################################################
# Script Name: conch-env-setup.sh
# Description: Automates Conch environment setup with one-click execution
# Core Features:
#   1. Check and install runtime dependencies (containerd & cloud-hypervisor)
#   2. Configure registry mirror and SSL skip-verify for image pulling
#   3. Pull builder and function images with customizable tags
#   4. Execute containerized offline builds and image unpacking
#
# MUST run in Conch project directory:
#   From Conch root: ./scripts/conch-env-setup.sh
#
# Usage:
#   install: Installs containerd and cloud-hypervisor only if they are missing.
#   pull: Configures registry access and pulls the required images.
#   build: Runs an offline compilation inside a container to keep your host clean.
#   process: Uses the compiled tool to unpack and analyze the target image.
#   all: Executes the full workflow (install → pull → build → process) automatically.
#   Customization: Use --build_image or --main_image to switch versions on the fly.
###############################################################################

###############################################################################
# Architecture Detection and Image Selection
# - x86_64  -> -x86 suffix for images, amd64 for containerd, cloud-hypervisor-static
# - aarch64 -> -aarch suffix for images, arm64 for containerd, cloud-hypervisor-static-aarch64
###############################################################################
ARCH=$(uname -m)
case $ARCH in
    x86_64)  
        ARCH_SUFFIX="x86"
        CNTD_ARCH="amd64"
        CLH_BINARY="cloud-hypervisor-static"
        ;;
    aarch64) 
        ARCH_SUFFIX="aarch"
        CNTD_ARCH="arm64"
        CLH_BINARY="cloud-hypervisor-static-aarch64"
        ;;
    *)       
        echo "Unsupported architecture: $ARCH"; exit 1 
        ;;
esac

# Image defaults with architecture suffix
B_IMG_DEFAULT="hub.oepkgs.net/conch/conch-builder:v0.1-${ARCH_SUFFIX}"
F_IMG_DEFAULT="hub.oepkgs.net/conch/openeuler:odd-${ARCH_SUFFIX}"

# Containerd version and download URL
CNTD_VER="2.2.1"
CNTD_TAR="containerd-${CNTD_VER}-linux-${CNTD_ARCH}.tar.gz"
CNTD_URL="https://github.com/containerd/containerd/releases/download/v${CNTD_VER}/${CNTD_TAR}"

# Cloud-Hypervisor version and download URL
CLH_VER="51"
CLH_URL="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v${CLH_VER}.0/${CLH_BINARY}"

show_help() {
    echo "Usage: $0 [COMMAND] [OPTIONS]"
    echo ""
    echo "Commands:"
    echo "  provisioning           Install cloud-hypervisor and containerd"
    echo "  pull                   Pull function image and run unpack"
    echo "  build                  Install containerd, pull builder, and run build"
    echo "  sdk                    Install Python SDK in editable mode"
    echo "  install                Quick setup (provisioning + pull + sdk)"
    echo "  all                    Run full flow (provisioning, pull, build, sdk)"
    echo "  help                   Show this help message"
    echo ""
    echo "Options:"
    echo "  --build_image=VALUE    Specify the builder image (default: $B_IMG_DEFAULT)"
    echo "  --main_image=VALUE     Specify the main/function image (default: $F_IMG_DEFAULT)"
}

BUILD_IMG=$B_IMG_DEFAULT
MAIN_IMG=$F_IMG_DEFAULT
COMMAND=$1
shift

for i in "$@"; do
    case $i in
        --build_image=*) BUILD_IMG="${i#*=}"; shift ;;
        --main_image=*)  MAIN_IMG="${i#*=}"; shift ;;
    esac
done

install_clh() {
    echo "--- Checking Cloud-Hypervisor ---"
    CLH_MIN_VER=51
    CLH_NEED_INSTALL=0
    CLH_BIN_PATH=""

    # Check if cloud-hypervisor exists in PATH and is a valid executable
    CLH_BIN_PATH=$(command -v cloud-hypervisor 2>/dev/null)
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
        echo "Downloading cloud-hypervisor v${CLH_VER}.0 for ${ARCH}..."
        echo "URL: $CLH_URL"
        wget --progress=bar:force "$CLH_URL" -O /usr/local/bin/cloud-hypervisor 2>&1
        if [ $? -ne 0 ]; then
            echo "Error: Failed to download cloud-hypervisor."
            echo "Manual download: https://github.com/cloud-hypervisor/cloud-hypervisor/releases"
            return 1
        fi
        chmod +x /usr/local/bin/cloud-hypervisor
        echo "cloud-hypervisor v${CLH_VER}.0 installed successfully for ${ARCH}."
    fi
}

install_containerd() {
    echo "--- Checking Containerd ---"
    if command -v containerd >/dev/null 2>&1; then
        echo "containerd already exists. Skipping."
    else
        echo "Installing containerd v$CNTD_VER for ${ARCH}..."
        if [ ! -f "$CNTD_TAR" ]; then
            echo "URL: $CNTD_URL"
            wget --progress=bar:force "$CNTD_URL" -O "$CNTD_TAR" 2>&1
            if [ $? -ne 0 ]; then
                echo "Error: Failed to download containerd."
                echo "Manual download: https://github.com/containerd/containerd/releases"
                rm -f "$CNTD_TAR"
                return 1
            fi
        fi
        rm -rf ./bin_tmp && mkdir ./bin_tmp
        tar -zxf "$CNTD_TAR" -C ./bin_tmp
        cp -f ./bin_tmp/bin/* /usr/local/bin/ && chmod +x /usr/local/bin/containerd* /usr/local/bin/ctr
        ln -sf /usr/local/bin/containerd /usr/bin/containerd
        ln -sf /usr/local/bin/ctr /usr/bin/ctr

        mkdir -p /etc/containerd
        /usr/local/bin/containerd config default > /etc/containerd/config.toml

        cat <<EOF > /etc/systemd/system/containerd.service
[Unit]
Description=containerd container runtime
Documentation=https://containerd.io
After=network.target local-fs.target dbus.service

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNOFILE=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload && systemctl enable --now containerd && systemctl restart containerd
        rm -rf ./bin_tmp
        rm -f "$CNTD_TAR"
        echo "containerd setup completed."
    fi
    hash -r
}

install_buildah_erofs() {
    echo "--- Installing Buildah and EROFS Utils ---"
    if command -v dnf >/dev/null 2>&1; then
        sudo dnf install -y buildah erofs-utils
    elif command -v yum >/dev/null 2>&1; then
        sudo yum install -y buildah erofs-utils
    elif command -v apt-get >/dev/null 2>&1; then
        sudo apt-get update
        sudo apt-get install -y buildah erofs-utils
    else
        echo "unsupported package manager; please install buildah and erofs-utils manually" >&2
        exit 1
    fi

    # Verify installation
    for bin in buildah mkfs.erofs; do
        if ! command -v "$bin" >/dev/null 2>&1; then
            echo "missing required command: $bin" >&2
            exit 1
        fi
    done
    echo "buildah and erofs-utils installed and verified successfully."
}

setup_certs() {
    local IMG=$1
    DOMAIN=$(echo "$IMG" | cut -d/ -f1)
    CERT_DIR="/etc/containerd/certs.d/$DOMAIN"
    if [ ! -d "$CERT_DIR" ]; then
        mkdir -p "$CERT_DIR"
        echo -e "server = \"https://$DOMAIN\"\n[host.\"https://$DOMAIN\"]\n  capabilities = [\"pull\", \"resolve\"]\n  skip_verify = true" > "$CERT_DIR/hosts.toml"
    fi
}

pull_builder() {
    echo "--- Pulling Builder Image ---"
    setup_certs "$BUILD_IMG"
    ctr -n default images pull "$BUILD_IMG"
}

pull_function() {
    echo "--- Pulling Function Image via conch ---"
    if [ -x "./bin/conch" ]; then
        ./bin/conch pull "$MAIN_IMG"
    else
        echo "Error: ./bin/conch executable not found."
        return 1
    fi
}

run_build() {
    echo "--- Compiling with $BUILD_IMG ---"
    ctr -n default run --rm --net-host \
      --mount type=bind,src=$(pwd),dst=/build,options=rbind:rw \
      --env GOPATH=/go \
      "$BUILD_IMG" \
      "build-$(date +%s)" \
      sh -c "cd /build && make build-offline"
}

install_sdk() {
    echo "--- Installing Python SDK ---"
    if [ -d "./sdk" ]; then
        pip install -e ./sdk --break-system-packages  --ignore-installed typing-extensions
        if [ $? -ne 0 ]; then
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
    else
        echo "Error: ./sdk directory not found."
        return 1
    fi
}

case "$COMMAND" in
    provisioning) install_clh && install_containerd && install_buildah_erofs ;;
    pull)    pull_function ;;
    build)   install_containerd && pull_builder && run_build ;;
    sdk)     install_sdk ;;
    install) install_clh && install_containerd && install_buildah_erofs && run_build && pull_function && install_sdk;;
    all)     install_clh && install_containerd && install_buildah_erofs && pull_builder && run_build && pull_function && install_sdk;;
    help|*)  show_help ;;
esac
