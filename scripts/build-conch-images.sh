#!/bin/bash
set -euo pipefail

echo "------------------------------------------------"
echo "   _____                 _                      "
echo "  / ____|               | |                     "
echo " | |     ___  _ __   ___| |__                   "
echo " | |    / _ \| '_ \ / __| '_ \                  "
echo " | |___| (_) | | | | (__| | | |                 "
echo "  \_____\___/|_| |_|\___|_| |_| Image Builder   "
echo "------------------------------------------------"

# ===================== 配置参数 =====================
TAG="build-test"
IMAGE_REG="hub.oepkgs.net/conch"
INDEX_NAME="${IMAGE_REG}/conch-claw:${TAG}"

VM_IMAGE="${IMAGE_REG}/conch-vm:${TAG}"
ROOTFS_IMAGE="${IMAGE_REG}/pmem-rootfs:${TAG}"

# 构建目录
BUILD_DIR="build-artifacts"
ALIGN_BYTES=$((2 * 1024 * 1024))

# 直接使用命令名，避免依赖 which
MKFS_EROFS="mkfs.erofs"
BUILDAH_CMD="buildah"

# 源文件路径
KERNEL_FILE="${BUILD_DIR}/bzImage"
RAW_FILE="${BUILD_DIR}/conch.initrd"
INPUT_PATH="conch-rootfs.tar"

# ===================== 颜色定义 =====================
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[0;33m'
RED='\033[0;31m'
NC='\033[0m'

# ===================== 工具函数 =====================
log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_step() { echo -e "${BLUE}[STEP $1]${NC} $2"; }

check_files() {
    local missing=()
    for file in "$@"; do
        [ ! -f "$file" ] && missing+=("$file")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        log_error "以下文件不存在:"
        printf '  - %s\n' "${missing[@]}"
        exit 1
    fi
}

check_tools() {
    local missing=()
    for tool in "$@"; do
        if ! command -v "$tool" &>/dev/null; then
            missing+=("$tool")
        fi
    done

    if [ ${#missing[@]} -gt 0 ]; then
        log_error "以下工具未安装:"
        for t in "${missing[@]}"; do
            echo "  - $t"
            if [ "$t" = "mkfs.erofs" ]; then
                echo "    安装方法: sudo apt install erofs-utils   或  sudo yum install erofs-utils"
            elif [ "$t" = "buildah" ]; then
                echo "    安装方法（Ubuntu/Debian）: sudo apt update && sudo apt install -y buildah"
                echo "    安装方法（CentOS/RHEL/AlmaLinux）: sudo yum install -y buildah"
                echo "    安装方法（Fedora）: sudo dnf install -y buildah"
                echo "    或参考官方指南: https://github.com/containers/buildah#installation"
            elif [ "$t" = "jq" ]; then
                echo "    安装方法（Ubuntu/Debian）: sudo apt install -y jq"
                echo "    安装方法（CentOS/RHEL/AlmaLinux）: sudo yum install -y jq"
                echo "    安装方法（Fedora）: sudo dnf install -y jq"
            elif [ "$t" = "xz" ]; then
                echo "    安装方法: 通常在 xz-utils 或 xz 包中, 如 sudo apt install -y xz-utils"
            fi
        done
        exit 1
    fi
}

# ===================== EROFS 转换相关 =====================
process_erofs() {
    local src_tar=$1
    local dest_erofs=$2
    echo -en "    转换至 $(basename "$dest_erofs")... "
    "$MKFS_EROFS" --tar=f --aufs -Enoinline_data "$dest_erofs" "$src_tar" >/dev/null 2>&1

    # 2MB 对齐
    local file_size
    file_size=$(stat -c%s "$dest_erofs")
    local aligned_size=$(( (file_size + ALIGN_BYTES - 1) / ALIGN_BYTES * ALIGN_BYTES ))
    [ "$aligned_size" -eq 0 ] && aligned_size=$ALIGN_BYTES
    truncate -s "$aligned_size" "$dest_erofs"

    echo -e "${GREEN}完成${NC}"
}

make_erofs_layers() {
    check_files "$INPUT_PATH"

    local WORK_DIR
    WORK_DIR=$(mktemp -d)
    mkdir -p "$BUILD_DIR"
    trap 'rm -rf "$WORK_DIR"' EXIT

    log_info "分析输入文件: $(basename "$INPUT_PATH")"

    # 处理 xz 压缩
    local CURRENT_TAR="$INPUT_PATH"
    if [[ "$INPUT_PATH" == *.xz ]]; then
        log_info "检测到 xz 压缩，正在解压..."
        xz -dc "$INPUT_PATH" > "$WORK_DIR/temp.tar"
        CURRENT_TAR="$WORK_DIR/temp.tar"
    fi

    # 预解压 manifest.json 判断是否多层
    tar -xf "$CURRENT_TAR" -C "$WORK_DIR" manifest.json 2>/dev/null || true

    ERFS_LAYERS=()  # 用于存储生成的 EROFS 文件

    if [ -f "$WORK_DIR/manifest.json" ]; then
        echo -e "${YELLOW}检测到 Docker Save 格式 (多层)${NC}"
        tar -xf "$CURRENT_TAR" -C "$WORK_DIR"
        local layers
        layers=$(jq -r '.[0].Layers[]' "$WORK_DIR/manifest.json")
        local n=0
        for layer_path in $layers; do
            local dest="$BUILD_DIR/layer${n}.erofs"
            process_erofs "$WORK_DIR/$layer_path" "$dest"
            ERFS_LAYERS+=("$dest")
            n=$((n+1))
        done
    else
        echo -e "${YELLOW}检测到单层 Rootfs 格式${NC}"
        local dest="$BUILD_DIR/rootfs.erofs"
        process_erofs "$CURRENT_TAR" "$dest"
        ERFS_LAYERS+=("$dest")
    fi

    log_success "EROFS 转换完成! 结果存放在: $BUILD_DIR"
    ls -lh "$BUILD_DIR"
}

# ===================== 构建 Docker 镜像 =====================
build_vm_image() {
    log_step "1" "构建 VM 镜像..."
    check_files "$KERNEL_FILE" "$RAW_FILE"

    local cid
    cid=$($BUILDAH_CMD from scratch)
    $BUILDAH_CMD copy "$cid" "$KERNEL_FILE" /boot/vmlinuz
    $BUILDAH_CMD copy "$cid" "$RAW_FILE" /data/conch.initrd
    $BUILDAH_CMD config --label "io.conch.type=combined" \
                        --label "io.conch.kernel=bzImage" \
                        --label "io.conch.initrd=present" \
                        "$cid"
    $BUILDAH_CMD commit "$cid" "$VM_IMAGE"
    $BUILDAH_CMD rm "$cid"
}

build_rootfs_image() {
    log_step "2" "构建 PMEM RootFS 镜像 (动态层)..."

    if [ ${#ERFS_LAYERS[@]} -eq 0 ]; then
        log_error "EROFS 层未生成，无法构建 RootFS 镜像"
        exit 1
    fi

    local cid
    cid=$($BUILDAH_CMD from scratch)

    for layer_file in "${ERFS_LAYERS[@]}"; do
        [ ! -f "$layer_file" ] && log_error "$layer_file 不存在"
        $BUILDAH_CMD copy "$cid" "$layer_file" "/$(basename "$layer_file")"
    done

    local layer_names
    layer_names=$(for f in "${ERFS_LAYERS[@]}"; do basename "$f"; done | paste -sd, -)
    $BUILDAH_CMD config --label "io.conch.type=pmem-rootfs" \
                       --label "io.conch.format=erofs" \
                       --label "io.conch.layers=$layer_names" \
                       --annotation "description=PMEM rootfs image containing ${layer_names}" \
                       "$cid"

    $BUILDAH_CMD commit "$cid" "$ROOTFS_IMAGE"
    $BUILDAH_CMD rm "$cid"
}

create_manifest_index() {
    log_step "3" "创建 Manifest Index 并整合镜像..."

    $BUILDAH_CMD manifest rm "$INDEX_NAME" &> /dev/null || true
    $BUILDAH_CMD manifest create "$INDEX_NAME"

    $BUILDAH_CMD manifest add \
        --annotation "io.conch.kind=rootfs" \
        --annotation "org.opencontainers.image.title=Base Rootfs Image" \
        "$INDEX_NAME" "$ROOTFS_IMAGE"

    $BUILDAH_CMD manifest add \
        --annotation "io.conch.kind=virtual-machine" \
        --annotation "org.opencontainers.image.title=Virtual Machine Base Image" \
        "$INDEX_NAME" "$VM_IMAGE"
}

print_summary() {
    echo ""
    echo "------------------------------------------------------"
    log_success "全流程完成！"
    echo "  最终清单镜像: $INDEX_NAME"
    echo "  包含组件: Kernel, Initrd, Rootfs(${ERFS_LAYERS[*]})"
    echo "  推送到仓库: buildah manifest push --all $INDEX_NAME"
    echo "------------------------------------------------------"
}

# ===================== 主流程 =====================
main() {
    log_info "开始构建镜像..."

    # 先检查工具
    check_tools "$MKFS_EROFS" "$BUILDAH_CMD" jq xz

    # 1. 使用 buildah 构建 rootfs 容器镜像
    log_info "使用 buildah 构建容器镜像..."
    $BUILDAH_CMD bud --isolation chroot --network host \
        --build-arg http_proxy=${http_proxy:-} \
        --build-arg https_proxy=${https_proxy:-} \
        -t conch-rootfs .

    # 2. 导出为 tar
    log_info "导出容器镜像为 tar 文件..."
    $BUILDAH_CMD push localhost/conch-rootfs:latest docker-archive:"$INPUT_PATH"

    # 3. 转换为 EROFS 格式
    make_erofs_layers "$@"

    # 4. 构建最终镜像
    build_vm_image
    build_rootfs_image
    create_manifest_index

    print_summary
}

main "$@"

