#!/bin/bash

# Attempt to find skopeo in the system PATH
SYSTEM_SKOPEO=$(command -v skopeo)

# Use the environment variable SKOPEO if set, otherwise use the system one
SKOPEO="${SKOPEO:-$SYSTEM_SKOPEO}"

# --- 2. Dependency & Path Check ---

if [ -z "$SKOPEO" ] || [ ! -x "$SKOPEO" ]; then
    echo "======================================================="
    echo " ERROR: 'skopeo' binary not found!"
    echo "======================================================="
    echo " To run this script, please:"
    echo " 1. Install skopeo: "
    echo "    - Ubuntu/Debian: sudo apt install skopeo"
    echo "    - CentOS/RHEL:   sudo yum install skopeo"
    echo " 2. OR provide a custom path via environment variable:"
    echo "    export SKOPEO=/path/to/your/skopeo"
    echo "    $0 $@"
    echo "======================================================="
    exit 1
fi

# Confirm which skopeo is being used
echo "Using skopeo from: $SKOPEO"

# --- Argument Parsing ---
BUNDLE_REF=$1
TARGET_DIR=$2

# Function to display usage instructions
usage() {
    echo "Usage: $0 <boot_index_reference> <target_directory>"
    echo "Example: $0 localhost/conch/sandbox-snapshot:latest ./vm_export_data"
    exit 1
}

# Validate arguments
if [ -z "$BUNDLE_REF" ] || [ -z "$TARGET_DIR" ]; then
    echo "Error: Missing required arguments."
    usage
fi

# Check for jq dependency
if ! command -v jq &> /dev/null; then
    echo "Error: 'jq' is not installed. Please install it (e.g., yum/apt install jq)."
    exit 1
fi

# Check if the target directory already exists and is not empty
if [ -d "$TARGET_DIR" ] && [ "$(ls -A "$TARGET_DIR" 2>/dev/null)" ]; then
    echo "======================================================="
    echo " ERROR: Target directory is not empty!"
    echo " Path: $(readlink -f "$TARGET_DIR")"
    echo "======================================================="
    echo " To prevent data corruption, the export has been aborted."
    echo " Please specify a new directory or clear the existing one."
    exit 1
fi

echo "-------------------------------------------------------"
echo "Initializing OCI Boot Index Extraction"
echo "Source: $BUNDLE_REF"
echo "Destination: $TARGET_DIR"
echo "-------------------------------------------------------"

# 1. Inspect the Boot Index from local storage
echo "[1/4] Resolving Boot Index Metadata..."
INDEX_JSON=$($SKOPEO inspect --raw containers-storage:"$BUNDLE_REF")

if [ $? -ne 0 ]; then
    echo "Error: Failed to locate boot index '$BUNDLE_REF' in containers-storage."
    exit 1
fi

# Extract member tags using jq
ROOTFS_TAG=$(echo "$INDEX_JSON" | jq -r '.annotations["io.conch.boot.rootfs.tag"] // empty')
SNAP_TAG=$(echo "$INDEX_JSON" | jq -r '.annotations["io.conch.boot.snapshot.tag"] // empty')

# Check if tags were successfully retrieved
if [ -z "$ROOTFS_TAG" ] || [ -z "$SNAP_TAG" ]; then
    echo "Error: Boot Index annotations are missing 'rootfs' or 'snapshot' tags."
    exit 1
fi

echo "-> Detected RootFS: $ROOTFS_TAG"
echo "-> Detected Snapshot: $SNAP_TAG"

# 2. Export images to standard OCI Layout
echo "[2/4] Exporting OCI Layouts..."
mkdir -p "$TARGET_DIR/rootfs" "$TARGET_DIR/snapshot"

echo "Downloading RootFS layers..."
$SKOPEO copy containers-storage:"$ROOTFS_TAG" oci:"$TARGET_DIR/rootfs":latest --quiet

echo "Downloading Snapshot layers..."
$SKOPEO copy containers-storage:"$SNAP_TAG" oci:"$TARGET_DIR/snapshot":latest --quiet

echo "-------------------------------------------------------"
echo "Success! Images exported to OCI format."
echo "RootFS path: $(readlink -f "$TARGET_DIR/rootfs")"
echo "Snapshot path: $(readlink -f "$TARGET_DIR/snapshot")"
echo "-------------------------------------------------------"
