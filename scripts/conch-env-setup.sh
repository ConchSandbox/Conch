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

B_IMG_DEFAULT="hub.oepkgs.net/conch/conch-builder:v0.1"
F_IMG_DEFAULT="hub.oepkgs.net/conch/conch-index:v0.1"
CNTD_VER="2.2.1"
CNTD_TAR="containerd-${CNTD_VER}-linux-amd64.tar.gz"
CNTD_URL="https://github.com/containerd/containerd/releases/download/v${CNTD_VER}/${CNTD_TAR}"

show_help() {
    echo "Usage: $0 [COMMAND] [OPTIONS]"
    echo ""
    echo "Commands:"
    echo "  install    Install containerd and cloud-hypervisor (skips if exist)"
    echo "  pull       Pull builder and main images"
    echo "  build      Run offline build process in container"
    echo "  process    Run conch-unpack processing"
    echo "  all        Run install, pull, build, and process in sequence"
    echo "  help       Show this help message"
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

install_runtime() {
    echo "--- Step 1: Checking Dependencies ---"
    if command -v cloud-hypervisor >/dev/null 2>&1; then
        echo "cloud-hypervisor already exists."
    else
        echo "Installing cloud-hypervisor..."
        wget -q https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v48.0/cloud-hypervisor-static -O /usr/local/bin/cloud-hypervisor
        if [ $? -ne 0 ]; then
            echo "Error: Failed to download cloud-hypervisor."
            echo "Manual download: https://github.com/cloud-hypervisor/cloud-hypervisor/releases"
            return 1
        fi
        chmod +x /usr/local/bin/cloud-hypervisor
    fi

    if command -v containerd >/dev/null 2>&1; then
        echo "containerd already exists. Skipping."
    else
        echo "Installing containerd v$CNTD_VER..."
        [ ! -f "$CNTD_TAR" ] && wget -q "$CNTD_URL"
        if [ $? -ne 0 ]; then
            echo "Error: Failed to download containerd."
            echo "Manual download: https://github.com/containerd/containerd/releases"
            return 1
        fi
        rm -rf ./bin_tmp && mkdir ./bin_tmp
        tar -zxf "$CNTD_TAR" -C ./bin_tmp
        cp -f ./bin_tmp/bin/* /usr/local/bin/ && chmod +x /usr/local/bin/containerd* /usr/local/bin/ctr
        ln -sf /usr/local/bin/containerd /usr/bin/containerd
        ln -sf /usr/local/bin/ctr /usr/bin/ctr

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

pull_images() {
    echo "--- Step 2: Pulling Images ---"
    echo "Builder: $BUILD_IMG"
    echo "Main:    $MAIN_IMG"

    for IMG in "$BUILD_IMG" "$MAIN_IMG"; do
        DOMAIN=$(echo "$IMG" | cut -d/ -f1)
        CERT_DIR="/etc/containerd/certs.d/$DOMAIN"
        if [ ! -d "$CERT_DIR" ]; then
            mkdir -p "$CERT_DIR"
            echo -e "server = \"https://$DOMAIN\"\n[host.\"https://$DOMAIN\"]\n  capabilities = [\"pull\", \"resolve\"]\n  skip_verify = true" > "$CERT_DIR/hosts.toml"
        fi
    done
    ctr -n default images pull "$BUILD_IMG"
    ctr -n default images pull "$MAIN_IMG"
}

run_build() {
    echo "--- Step 3: Compiling with $BUILD_IMG ---"
    ctr -n default run --rm --net-host \
      --mount type=bind,src=$(pwd),dst=/build,options=rbind:rw \
      --env GOPATH=/go \
      "$BUILD_IMG" \
      "build-$(date +%s)" \
      sh -c "cd /build && make build-offline"
}

run_unpack() {
    echo "--- Step 4: Unpacking $MAIN_IMG ---"
    if [ -x "./bin/conch-unpack" ]; then
        ./bin/conch-unpack "$MAIN_IMG"
    else
        echo "Error: ./bin/conch-unpack executable not found."
        return 1
    fi
}

case "$COMMAND" in
    install) install_runtime ;;
    pull)    pull_images ;;
    build)   run_build ;;
    process) run_unpack ;;
    all)     install_runtime && pull_images && run_build && run_unpack ;;
    help|*)  show_help ;;
esac