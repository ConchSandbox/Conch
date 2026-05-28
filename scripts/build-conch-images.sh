#!/bin/bash
set -euo pipefail

echo "------------------------------------------------"
echo "   _____                 _                      "
echo "  / ____|               | |                     "
echo " | |     ___  _ __   ___| |__                   "
echo " | |    / _ \\| '_ \\ / __| '_ \\                  "
echo " | |___| (_) | | | | (__| | | |                 "
echo "  \\_____\\___/|_| |_|\\___|_| |_| Image Builder   "
echo "------------------------------------------------"

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

TAG_DEFAULT="build-test"
IMAGE_REG_DEFAULT="hub.oepkgs.net/conch"
MODE_DEFAULT="full"
BUILD_DIR_DEFAULT="${REPO_ROOT}/build-artifacts"
CONTEXT_DIR_DEFAULT="${REPO_ROOT}"
DOCKERFILE_DEFAULT="Dockerfile"

GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[0;33m'
RED='\033[0;31m'
NC='\033[0m'

ALIGN_BYTES=$((2 * 1024 * 1024))
MKFS_EROFS="mkfs.erofs"
BUILDAH_CMD="buildah"
LOCAL_ROOTFS_BUILD_IMAGE="localhost/conch-rootfs:latest"

log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1" >&2; }
log_step() { echo -e "${BLUE}[STEP $1]${NC} $2"; }

run_cmd() {
    if [ "${DRY_RUN}" = "1" ]; then
        printf '[DRY-RUN] ' >&2
        printf '%q ' "$@" >&2
        printf '\n' >&2
        return 0
    fi
    "$@"
}

usage() {
    cat <<EOF
用法: $0 [选项]

构建/打包 Conch 镜像，支持多种复用场景，不必每次都从 Dockerfile 全量开始。

模式:
  full         从 Dockerfile 开始，全量执行：buildah build -> tar -> EROFS -> VM/RootFS/Index
  erofs-only   仅把现有 rootfs tar 转为 EROFS，不生成最终镜像
  pack         目录里已有 .erofs 时直接打包成 VM/RootFS/Index
  vm-only      只更新 bzImage/conch.initrd，复用已有 RootFS 镜像重新生成 Index
  rootfs-only  只更新 .erofs，复用已有 VM 镜像重新生成 Index

选项:
  -m, --mode MODE         构建模式 (默认: ${MODE_DEFAULT})
  -t, --tag TAG           镜像标签 (默认: ${TAG_DEFAULT})
  -r, --registry URL      镜像仓库前缀 (默认: ${IMAGE_REG_DEFAULT})
      --index-repo REPO   Index 镜像仓库名，不含 tag (默认: <registry>/conch-claw)
      --vm-repo REPO      Kernel 镜像仓库名，不含 tag (默认: <registry>/kernel)
      --rootfs-repo REPO  RootFS 镜像仓库名，不含 tag (默认: <registry>/pmem-rootfs)
      --build-dir DIR     构建产物目录 (默认: ${BUILD_DIR_DEFAULT})
      --context-dir DIR   Dockerfile 构建上下文目录 (默认: ${CONTEXT_DIR_DEFAULT})
      --dockerfile PATH   Dockerfile 路径；相对路径按 context-dir 解析
                         未指定时，优先查找 <build-dir>/Dockerfile，其次查找 <context-dir>/Dockerfile
      --input-tar PATH    Rootfs tar/docker-archive 路径 (默认: <build-dir>/conch-rootfs.tar)
      --dry-run           仅打印将执行的步骤和命令，不真正执行
  -h, --help              显示帮助信息

默认文件位置:
  仓库根目录: ${REPO_ROOT}
  Dockerfile: ${BUILD_DIR_DEFAULT}/${DOCKERFILE_DEFAULT} 或 ${CONTEXT_DIR_DEFAULT}/${DOCKERFILE_DEFAULT}
  bzImage:    ${BUILD_DIR_DEFAULT}/bzImage
  conch.initrd: ${BUILD_DIR_DEFAULT}/conch.initrd
  EROFS 层:   ${BUILD_DIR_DEFAULT}/rootfs.erofs 或 ${BUILD_DIR_DEFAULT}/layer*.erofs
  Rootfs tar: ${BUILD_DIR_DEFAULT}/conch-rootfs.tar

常见场景:
  1. 首次全量构建
     $0 --mode full --tag dev

  2. 已有 .erofs，直接打包
     $0 --mode pack --build-dir ./build-artifacts --tag dev

  3. 只改了 bzImage / conch.initrd
     $0 --mode vm-only --tag dev

  4. 只想把 tar 转成 EROFS
     $0 --mode erofs-only --input-tar ./build-artifacts/conch-rootfs.tar

运行前请确认:
  - full 模式: 默认会先在 build-dir 下查找 Dockerfile，再回退到 context-dir；同时 build-dir 下有 bzImage 和 conch.initrd
  - pack 模式: build-dir 下已有 .erofs + bzImage + conch.initrd
  - vm-only 模式: build-dir 下有 bzImage + conch.initrd，且本地已有 RootFS 镜像
  - rootfs-only 模式: build-dir 下已有 .erofs，且本地已有 VM 镜像
EOF
}

die_with_usage() {
    local reason=$1
    echo "" >&2
    log_error "原因: ${reason}"
    echo "" >&2
    usage >&2
    exit 1
}

check_tools() {
    if [ "${DRY_RUN}" = "1" ]; then
        log_warn "dry-run 模式下跳过工具存在性检查"
        return 0
    fi
    local missing=()
    for tool in "$@"; do
        if ! command -v "$tool" >/dev/null 2>&1; then
            missing+=("$tool")
        fi
    done

    if [ ${#missing[@]} -gt 0 ]; then
        log_error "以下工具未安装:"
        for t in "${missing[@]}"; do
            echo "  - $t" >&2
        done
        echo "" >&2
        usage >&2
        exit 1
    fi
}

check_files() {
    local missing=()
    for file in "$@"; do
        [ ! -f "$file" ] && missing+=("$file")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        log_error "以下文件不存在:"
        printf '  - %s\n' "${missing[@]}" >&2
        echo "" >&2
        usage >&2
        exit 1
    fi
}

resolve_dockerfile_path() {
    if [[ "$DOCKERFILE_PATH" = /* ]]; then
        printf '%s\n' "$DOCKERFILE_PATH"
    elif [ "${DOCKERFILE_PATH_SET}" = "1" ]; then
        printf '%s\n' "${CONTEXT_DIR}/${DOCKERFILE_PATH}"
    elif [ -f "${BUILD_DIR}/${DOCKERFILE_DEFAULT}" ]; then
        printf '%s\n' "${BUILD_DIR}/${DOCKERFILE_DEFAULT}"
    else
        printf '%s\n' "${CONTEXT_DIR}/${DOCKERFILE_DEFAULT}"
    fi
}

check_local_image() {
    local image=$1
    local label=$2
    if [ "${DRY_RUN}" = "1" ]; then
        log_warn "dry-run 模式下跳过本地镜像检查: ${label} ${image}"
        return 0
    fi
    if ! "$BUILDAH_CMD" images --format '{{.Name}}:{{.Tag}}' | grep -Fxq "$image"; then
        die_with_usage "${label}不存在: ${image}。请先执行能生成它的模式，或调整 --tag/--*-repo 参数。"
    fi
}

process_erofs() {
    local src_tar=$1
    local dest_erofs=$2
    echo -en "    转换至 $(basename "$dest_erofs")... "
    if ! run_cmd "$MKFS_EROFS" --tar=f --aufs -Enoinline_data "$dest_erofs" "$src_tar"; then
        echo ""
        die_with_usage "EROFS 转换失败: $src_tar -> $dest_erofs"
    fi

    if [ "${DRY_RUN}" = "1" ]; then
        log_success "已预演"
        return 0
    fi

    local file_size
    file_size=$(stat -c%s "$dest_erofs")
    local aligned_size=$(((file_size + ALIGN_BYTES - 1) / ALIGN_BYTES * ALIGN_BYTES))
    [ "$aligned_size" -eq 0 ] && aligned_size=$ALIGN_BYTES
    run_cmd truncate -s "$aligned_size" "$dest_erofs"

    log_success "完成"
}

make_erofs_layers_from_input() {
    check_files "$INPUT_PATH"

    if [ "${DRY_RUN}" = "1" ]; then
        mkdir -p "$BUILD_DIR"
        log_info "dry-run: 将从 ${INPUT_PATH} 生成 EROFS 层到 ${BUILD_DIR}"
        EROFS_LAYERS=("${BUILD_DIR}/rootfs.erofs")
        return 0
    fi

    local work_dir
    work_dir=$(mktemp -d)
    mkdir -p "$BUILD_DIR"
    trap 'rm -rf "$work_dir"' RETURN

    log_info "分析输入文件: $(basename "$INPUT_PATH")"

    local current_tar="$INPUT_PATH"
    if [[ "$INPUT_PATH" == *.xz ]]; then
        log_info "检测到 xz 压缩，正在解压..."
        xz -dc "$INPUT_PATH" > "$work_dir/temp.tar"
        current_tar="$work_dir/temp.tar"
    fi

    tar -xf "$current_tar" -C "$work_dir" manifest.json 2>/dev/null || true

    EROFS_LAYERS=()

    if [ -f "$work_dir/manifest.json" ]; then
        echo -e "${YELLOW}检测到 Docker Save 格式 (多层)${NC}"
        tar -xf "$current_tar" -C "$work_dir"
        local n=0
        while IFS= read -r layer_path; do
            [ -z "$layer_path" ] && continue
            local dest="${BUILD_DIR}/layer${n}.erofs"
            process_erofs "${work_dir}/${layer_path}" "$dest"
            EROFS_LAYERS+=("$dest")
            n=$((n + 1))
        done < <(jq -r '.[0].Layers[]' "$work_dir/manifest.json")
    else
        echo -e "${YELLOW}检测到单层 Rootfs 格式${NC}"
        local dest="${BUILD_DIR}/rootfs.erofs"
        process_erofs "$current_tar" "$dest"
        EROFS_LAYERS+=("$dest")
    fi

    log_success "EROFS 转换完成! 结果存放在: $BUILD_DIR"
}

collect_existing_erofs_layers() {
    EROFS_LAYERS=()
    shopt -s nullglob
    local erofs_files=("${BUILD_DIR}"/*.erofs)
    shopt -u nullglob

    if [ ${#erofs_files[@]} -eq 0 ]; then
        die_with_usage "未在 ${BUILD_DIR} 中找到任何 .erofs 文件。pack/rootfs-only 模式要求预先存在 EROFS 层。"
    fi

    IFS=$'\n' read -r -d '' -a EROFS_LAYERS < <(printf '%s\n' "${erofs_files[@]}" | sort && printf '\0')
    log_info "复用已有 EROFS 层:"
    printf '  - %s\n' "${EROFS_LAYERS[@]}"
}

build_rootfs_archive_from_dockerfile() {
    local dockerfile_abs
    dockerfile_abs=$(resolve_dockerfile_path)
    check_files "$dockerfile_abs" "$KERNEL_FILE" "$RAW_FILE"

    mkdir -p "$BUILD_DIR"

    log_step "1" "使用 Dockerfile 构建 rootfs 容器镜像..."
    run_cmd "$BUILDAH_CMD" bud --isolation chroot --network host \
        --build-arg http_proxy="${http_proxy:-}" \
        --build-arg https_proxy="${https_proxy:-}" \
        -f "$dockerfile_abs" \
        -t "$LOCAL_ROOTFS_BUILD_IMAGE" \
        "$CONTEXT_DIR"

    log_step "2" "导出容器镜像为 tar 文件..."
    run_cmd "$BUILDAH_CMD" push "$LOCAL_ROOTFS_BUILD_IMAGE" "docker-archive:${INPUT_PATH}"
}

build_vm_image() {
    log_step "VM" "构建 VM 镜像..."
    check_files "$KERNEL_FILE" "$RAW_FILE"

    if [ "${DRY_RUN}" = "1" ]; then
        log_info "dry-run: 将使用 ${KERNEL_FILE} 和 ${RAW_FILE} 构建 ${VM_IMAGE}"
        printf '[DRY-RUN] %q from scratch\n' "$BUILDAH_CMD"
        printf '[DRY-RUN] %q copy <cid> %q /boot/vmlinuz\n' "$BUILDAH_CMD" "$KERNEL_FILE"
        printf '[DRY-RUN] %q copy <cid> %q /data/conch.initrd\n' "$BUILDAH_CMD" "$RAW_FILE"
        printf '[DRY-RUN] %q config <cid> ...\n' "$BUILDAH_CMD"
        printf '[DRY-RUN] %q commit <cid> %q\n' "$BUILDAH_CMD" "$VM_IMAGE"
        printf '[DRY-RUN] %q rm <cid>\n' "$BUILDAH_CMD"
        return 0
    fi

    local cid
    cid=$("$BUILDAH_CMD" from scratch)
    "$BUILDAH_CMD" copy "$cid" "$KERNEL_FILE" /boot/vmlinuz
    "$BUILDAH_CMD" copy "$cid" "$RAW_FILE" /data/conch.initrd
    "$BUILDAH_CMD" config --label "io.conch.type=combined" \
        --label "io.conch.kernel=bzImage" \
        --label "io.conch.initrd=present" \
        "$cid"
    "$BUILDAH_CMD" commit "$cid" "$VM_IMAGE"
    "$BUILDAH_CMD" rm "$cid"
}

build_rootfs_image() {
    log_step "ROOTFS" "构建 PMEM RootFS 镜像 (基于 EROFS 层)..."

    if [ ${#EROFS_LAYERS[@]} -eq 0 ]; then
        die_with_usage "没有可用的 EROFS 层，无法构建 RootFS 镜像。"
    fi

    if [ "${DRY_RUN}" = "1" ]; then
        log_info "dry-run: 将使用 ${#EROFS_LAYERS[@]} 个 EROFS 层构建 ${ROOTFS_IMAGE}"
        printf '[DRY-RUN] %q from scratch\n' "$BUILDAH_CMD"
        for layer_file in "${EROFS_LAYERS[@]}"; do
            printf '[DRY-RUN] %q copy <cid> %q /%s\n' "$BUILDAH_CMD" "$layer_file" "$(basename "$layer_file")"
        done
        printf '[DRY-RUN] %q config <cid> ...\n' "$BUILDAH_CMD"
        printf '[DRY-RUN] %q commit <cid> %q\n' "$BUILDAH_CMD" "$ROOTFS_IMAGE"
        printf '[DRY-RUN] %q rm <cid>\n' "$BUILDAH_CMD"
        return 0
    fi

    local cid
    cid=$("$BUILDAH_CMD" from scratch)

    for layer_file in "${EROFS_LAYERS[@]}"; do
        [ -f "$layer_file" ] || die_with_usage "${layer_file} 不存在"
        "$BUILDAH_CMD" copy "$cid" "$layer_file" "/$(basename "$layer_file")"
    done

    local layer_names
    layer_names=$(for f in "${EROFS_LAYERS[@]}"; do basename "$f"; done | paste -sd, -)
    "$BUILDAH_CMD" config --label "io.conch.type=pmem-rootfs" \
        --label "io.conch.format=erofs" \
        --label "io.conch.layers=${layer_names}" \
        --annotation "description=PMEM rootfs image containing ${layer_names}" \
        "$cid"

    "$BUILDAH_CMD" commit "$cid" "$ROOTFS_IMAGE"
    "$BUILDAH_CMD" rm "$cid"
}

create_manifest_index() {
    log_step "INDEX" "创建 Manifest Index 并整合镜像..."

    if [ "${DRY_RUN}" = "1" ]; then
        log_info "dry-run: 将创建并更新 manifest index ${INDEX_NAME}"
        printf '[DRY-RUN] %q manifest rm %q\n' "$BUILDAH_CMD" "$INDEX_NAME"
        printf '[DRY-RUN] %q manifest create %q\n' "$BUILDAH_CMD" "$INDEX_NAME"
        printf '[DRY-RUN] %q manifest add --annotation io.conch.kind=rootfs %q %q\n' "$BUILDAH_CMD" "$INDEX_NAME" "$ROOTFS_IMAGE"
        printf '[DRY-RUN] %q manifest add --annotation io.conch.kind=sandbox %q %q\n' "$BUILDAH_CMD" "$INDEX_NAME" "$VM_IMAGE"
        return 0
    fi

    "$BUILDAH_CMD" manifest rm "$INDEX_NAME" >/dev/null 2>&1 || true
    "$BUILDAH_CMD" manifest create "$INDEX_NAME"

    "$BUILDAH_CMD" manifest add \
        --annotation "io.conch.kind=rootfs" \
        --annotation "org.opencontainers.image.title=Base Rootfs Image" \
        "$INDEX_NAME" "$ROOTFS_IMAGE"

    "$BUILDAH_CMD" manifest add \
        --annotation "io.conch.kind=sandbox" \
        --annotation "org.opencontainers.image.title=Sandbox Base Image" \
        "$INDEX_NAME" "$VM_IMAGE"
}

print_config_summary() {
    echo ""
    log_info "构建配置:"
    echo "  模式:        ${MODE}"
    echo "  仓库:        ${IMAGE_REG}"
    echo "  标签:        ${TAG}"
    echo "  Index:       ${INDEX_NAME}"
    echo "  VM:          ${VM_IMAGE}"
    echo "  RootFS:      ${ROOTFS_IMAGE}"
    echo "  build-dir:   ${BUILD_DIR}"
    echo "  context-dir: ${CONTEXT_DIR}"
    echo "  dockerfile:  $(resolve_dockerfile_path)"
    echo "  input-tar:   ${INPUT_PATH}"
    echo "  dry-run:     ${DRY_RUN}"
    echo ""
}

print_summary() {
    echo ""
    echo "------------------------------------------------------"
    log_success "流程完成！"
    echo "  最终清单镜像: $INDEX_NAME"
    if [ ${#EROFS_LAYERS[@]} -gt 0 ]; then
        echo "  EROFS 层: ${EROFS_LAYERS[*]}"
    fi
    echo "  推送命令: buildah manifest push --all $INDEX_NAME"
    echo "------------------------------------------------------"
}

validate_mode() {
    case "$MODE" in
        full)
            check_tools "$BUILDAH_CMD" "$MKFS_EROFS" jq xz
            check_files "$(resolve_dockerfile_path)" "$KERNEL_FILE" "$RAW_FILE"
            ;;
        erofs-only)
            check_tools "$MKFS_EROFS" jq xz
            check_files "$INPUT_PATH"
            ;;
        pack)
            check_tools "$BUILDAH_CMD"
            check_files "$KERNEL_FILE" "$RAW_FILE"
            ;;
        vm-only)
            check_tools "$BUILDAH_CMD"
            check_files "$KERNEL_FILE" "$RAW_FILE"
            check_local_image "$ROOTFS_IMAGE" "RootFS 镜像"
            ;;
        rootfs-only)
            check_tools "$BUILDAH_CMD"
            check_local_image "$VM_IMAGE" "VM 镜像"
            ;;
        *)
            die_with_usage "不支持的模式: ${MODE}"
            ;;
    esac
}

TAG="${TAG:-$TAG_DEFAULT}"
IMAGE_REG="${IMAGE_REG:-$IMAGE_REG_DEFAULT}"
MODE="${MODE:-$MODE_DEFAULT}"
BUILD_DIR="${BUILD_DIR:-$BUILD_DIR_DEFAULT}"
CONTEXT_DIR="${CONTEXT_DIR:-$CONTEXT_DIR_DEFAULT}"
DOCKERFILE_PATH="${DOCKERFILE_PATH:-$DOCKERFILE_DEFAULT}"
DOCKERFILE_PATH_SET=0
INPUT_PATH="${INPUT_PATH:-}"
INDEX_REPO="${INDEX_REPO:-}"
VM_REPO="${VM_REPO:-}"
ROOTFS_REPO="${ROOTFS_REPO:-}"
DRY_RUN="${DRY_RUN:-0}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -m|--mode)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            MODE="$2"
            shift 2
            ;;
        -t|--tag)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            TAG="$2"
            shift 2
            ;;
        -r|--registry)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            IMAGE_REG="$2"
            shift 2
            ;;
        --index-repo)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            INDEX_REPO="$2"
            shift 2
            ;;
        --vm-repo)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            VM_REPO="$2"
            shift 2
            ;;
        --rootfs-repo)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            ROOTFS_REPO="$2"
            shift 2
            ;;
        --build-dir)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            BUILD_DIR="$2"
            shift 2
            ;;
        --context-dir)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            CONTEXT_DIR="$2"
            shift 2
            ;;
        --dockerfile)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            DOCKERFILE_PATH="$2"
            DOCKERFILE_PATH_SET=1
            shift 2
            ;;
        --input-tar)
            [ $# -ge 2 ] || die_with_usage "缺少参数: $1"
            INPUT_PATH="$2"
            shift 2
            ;;
        --dry-run)
            DRY_RUN=1
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            die_with_usage "未知参数: $1"
            ;;
    esac
done

BUILD_DIR=$(cd "$BUILD_DIR" 2>/dev/null && pwd || printf '%s\n' "$BUILD_DIR")
CONTEXT_DIR=$(cd "$CONTEXT_DIR" 2>/dev/null && pwd || printf '%s\n' "$CONTEXT_DIR")
[ -n "$INPUT_PATH" ] || INPUT_PATH="${BUILD_DIR}/conch-rootfs.tar"
if [[ "$INPUT_PATH" != /* ]]; then
    INPUT_PATH="${REPO_ROOT}/${INPUT_PATH}"
fi

[ -n "$INDEX_REPO" ] || INDEX_REPO="${IMAGE_REG}/conch-claw"
[ -n "$VM_REPO" ] || VM_REPO="${IMAGE_REG}/kernel"
[ -n "$ROOTFS_REPO" ] || ROOTFS_REPO="${IMAGE_REG}/pmem-rootfs"

INDEX_NAME="${INDEX_REPO}:${TAG}"
VM_IMAGE="${VM_REPO}:${TAG}"
ROOTFS_IMAGE="${ROOTFS_REPO}:${TAG}"
KERNEL_FILE="${BUILD_DIR}/bzImage"
RAW_FILE="${BUILD_DIR}/conch.initrd"
EROFS_LAYERS=()

main() {
    print_config_summary
    validate_mode

    case "$MODE" in
        full)
            build_rootfs_archive_from_dockerfile
            make_erofs_layers_from_input
            build_vm_image
            build_rootfs_image
            create_manifest_index
            print_summary
            ;;
        erofs-only)
            make_erofs_layers_from_input
            print_summary
            ;;
        pack)
            collect_existing_erofs_layers
            build_vm_image
            build_rootfs_image
            create_manifest_index
            print_summary
            ;;
        vm-only)
            build_vm_image
            create_manifest_index
            print_summary
            ;;
        rootfs-only)
            collect_existing_erofs_layers
            build_rootfs_image
            create_manifest_index
            print_summary
            ;;
    esac
}

main "$@"
