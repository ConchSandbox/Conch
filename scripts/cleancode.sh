#!/usr/bin/env bash
set -o pipefail
set -o errexit

# Directories to exclude from scanning (extendable)
declare -a EXCLUDE_DIRS=(
  ".git"
  "venv"
  "__pycache__"
  "build"
  "dist"
  ".pytest_cache"
  "node_modules"
  "bin"
  "api/go_proto"
  "api/py_proto"
)

# File suffixes to process (extendable)
declare -a TARGET_SUFFIXES=(
  "py" "sh" "yaml" "yml" "md" "txt"
  "cfg" "ini" "json" "toml" "go" "proto"
)

# Basic configuration
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
VERBOSE=0       # General verbose output (info level)
DEBUG_MODE=0    # Debug mode (debug level, disabled by default)
DRY_RUN=0
ENABLE_BACKUP=0 # Backup disabled by default
BACKUP_ROOT="${PROJECT_ROOT}/.clean_backup"
EXIT_CODE=0

# Logging functions (with level control)
log_info() {
  if [[ $VERBOSE -eq 1 ]]; then
    echo -e "\033[32m[INFO]  $(date '+%Y-%m-%d %H:%M:%S')  $*\033[0m"
  fi
}

log_debug() {
  if [[ $DEBUG_MODE -eq 1 ]]; then
    echo -e "\033[36m[DEBUG] $(date '+%Y-%m-%d %H:%M:%S')  $*\033[0m"
  fi
}

log_error() {
  echo -e "\033[31m[ERROR] $(date '+%Y-%m-%d %H:%M:%S')  $*\033[0m" >&2
  EXIT_CODE=1
}

check_file_permission() {
  local file="$1"
  if [[ ! -f "$file" ]]; then
    log_error "File does not exist: $file"
    return 1
  fi
  if [[ ! -r "$file" ]]; then
    log_error "No read permission for file: $file"
    return 1
  fi
  if [[ ! -w "$file" && $DRY_RUN -eq 0 ]]; then
    log_error "No write permission for file: $file"
    return 1
  fi
  return 0
}

# Detection function: Compatible with EulerOS sed syntax
has_trailing_whitespace() {
  local file="$1"

  if ! check_file_permission "$file"; then
    return 1
  fi

  # Skip empty files
  if [[ ! -s "$file" ]]; then
    return 1
  fi

  # Check trailing spaces/tabs/full-width spaces
  local space_lines=$(sed -n '/[ \t　]$/p' "$file" | wc -l)
  if [[ $space_lines -gt 0 ]]; then
    return 0
  fi

  # Check trailing CR (from CRLF line endings)
  local cr_lines=$(sed -n '/\r$/p' "$file" | wc -l)
  if [[ $cr_lines -gt 0 ]]; then
    return 0
  fi

  # No trailing whitespace found
  return 1
}

# Cleanup function with backup toggle
clean_file() {
  local file="$1"
  local rel_path="${file#${PROJECT_ROOT}/}"

  if ! check_file_permission "$file"; then
    return 1
  fi

  if [[ $DRY_RUN -eq 1 ]]; then
    log_info "Dry run: Need cleanup - ${rel_path}"
    return 0
  fi

  log_info "Cleaning - ${rel_path}"

  if [[ $ENABLE_BACKUP -eq 1 ]]; then
    # Create centralized backup directory
    mkdir -p "$BACKUP_ROOT"
    # Backup file with original directory structure
    local backup_file="${BACKUP_ROOT}/${rel_path}.bak"
    mkdir -p "$(dirname "$backup_file")"
    if cp --preserve=all "$file" "$backup_file" 2>/dev/null; then
      log_debug "Backed up to: ${backup_file}"
    else
      log_error "Failed to backup file: $file"
      return 1
    fi
  fi

  if [[ "$(uname)" == "Darwin" ]]; then
    if ! sed -i '' -e 's/\r$//' -e 's/[ \t　]*$//' "$file" 2>/dev/null; then
      log_error "sed processing failed for file: $file"
      return 1
    fi
  else
    if ! sed -i -e 's/\r$//' -e 's/[ \t　]*$//' "$file" 2>/dev/null; then
      log_error "sed processing failed for file: $file"
      return 1
    fi
  fi

  return 0
}

# Argument parsing (with backup & debug toggle)
parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --dir)
        if [[ -d "$2" ]]; then
          PROJECT_ROOT="$(cd "$2" && pwd -P)"
          BACKUP_ROOT="${PROJECT_ROOT}/.clean_backup"
        else
          log_error "Specified directory does not exist: $2"
          exit 1
        fi
        shift 2
        ;;
      --verbose|-v)
        VERBOSE=1
        shift
        ;;
      --debug)
        DEBUG_MODE=1
        shift
        ;;
      --dry-run|-n)
        DRY_RUN=1
        shift
        ;;
      --backup)
        ENABLE_BACKUP=1
        shift
        ;;
      --no-backup)
        ENABLE_BACKUP=0
        shift
        ;;
      --help|-h)
        cat << HELP
Usage: ${0##*/} [OPTIONS]
EulerOS Optimized: Clean trailing whitespace and standardize line endings

Options:
  --dir DIR       Specify project root directory (default: parent directory of script)
  --verbose|-v    Show info-level logging (summary information)
  --debug         Show debug-level logging (detailed file checking, backup paths)
  --dry-run|-n    Only detect issues without making changes
  --backup        Enable file backup (save to ${PROJECT_ROOT}/.clean_backup)
  --no-backup     Disable file backup (default)
  --help|-h       Show this help message and exit

Key Features:
  1. Compatible with EulerOS/Linux/macOS sed syntax
  2. Detect/clean trailing spaces, tabs, full-width spaces and CR
  3. Precise file counting (no overcounting/undercounting)
  4. Optional backup (--backup) for data safety
HELP
        exit 0
        ;;
      *)
        log_error "Unknown argument: $1"
        exit 1
        ;;
    esac
  done

  if [[ ! -d "$PROJECT_ROOT" ]]; then
    log_error "Project root directory does not exist: $PROJECT_ROOT"
    exit 1
  fi
}

# Main function: Restructured counting logic
main() {
  parse_args "$@"
  log_info "Starting project scan: $PROJECT_ROOT"

  # Status notifications (only in verbose mode)
  if [[ $VERBOSE -eq 1 ]]; then
    if [[ $ENABLE_BACKUP -eq 1 ]]; then
      log_info "Backup enabled (backup directory: ${BACKUP_ROOT})"
    else
      log_info "Backup disabled (use --backup to enable data safety)"
    fi
    if [[ $DEBUG_MODE -eq 1 ]]; then
      log_info "Debug mode enabled (detailed logging)"
    fi
  fi

  # Build find command
  local find_cmd=(
    find "$PROJECT_ROOT"
    \(
  )
  local first_exclude=1
  for dir in "${EXCLUDE_DIRS[@]}"; do
    local exclude_path="${PROJECT_ROOT}/${dir}"
    if [[ $first_exclude -eq 1 ]]; then
      find_cmd+=(-path "$exclude_path")
      first_exclude=0
    else
      find_cmd+=(-o -path "$exclude_path")
    fi
  done
  find_cmd+=(
    \) -prune -o
    \(
  )
  local first_suffix=1
  for suffix in "${TARGET_SUFFIXES[@]}"; do
    if [[ $first_suffix -eq 1 ]]; then
      find_cmd+=(-name "*.$suffix")
      first_suffix=0
    else
      find_cmd+=(-o -name "*.$suffix")
    fi
  done
  find_cmd+=(
    \) -type f -print0
  )

  # Count total files to process
  local total_files=$("${find_cmd[@]}" | xargs -0 -I {} echo 1 | wc -l)
  log_info "Total files scanned: $total_files"

  local cleanup_count=0
  local cleanup_files=()

  # Process files
  while IFS= read -r -d '' file; do
    [[ -z "$file" || ! -f "$file" ]] && continue
    log_debug "Checking file: $file"

    if has_trailing_whitespace "$file"; then
      if clean_file "$file"; then
        cleanup_files+=("$file")
      fi
    fi
  done < <("${find_cmd[@]}")

  # Calculate actual cleanup count
  cleanup_count=${#cleanup_files[@]}

  # Output summary
  echo -e "\n==================== Cleanup Summary ===================="
  echo "Total files scanned:  $total_files"
  if [[ $DRY_RUN -eq 1 ]]; then
    echo "Files needing cleanup:  $cleanup_count"
  else
    echo "Files cleaned:  $cleanup_count"
    if [[ $ENABLE_BACKUP -eq 1 ]]; then
      echo "Note: All backups stored in ${BACKUP_ROOT}"
    else
      echo "Note: Backup disabled (use --backup to save original files)"
    fi
  fi
  echo "=========================================================="

  # Output cleanup list
  if [[ ($VERBOSE -eq 1 || $DEBUG_MODE -eq 1) && $cleanup_count -gt 0 ]]; then
    echo -e "\n===== $(if [[ $DRY_RUN -eq 1 ]]; then echo "List of Files Needing Cleanup"; else echo "List of Files Processed"; fi) ====="
    for f in "${cleanup_files[@]}"; do
      echo "- ${f#${PROJECT_ROOT}/}"
    done
  fi

  log_info "Operation completed successfully"
  exit $EXIT_CODE
}

# Start main execution
main "$@"
