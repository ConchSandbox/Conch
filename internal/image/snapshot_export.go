package image

import (
	"context"
	"fmt"
)

// SnapshotExportRequest is the unified input for exporting a snapshot-backed
// sandbox image from an existing rootfs snapshot or sandbox.
type SnapshotExportRequest struct {
	BootIndexTag     string
	ConfigPath       string
	ConchAPIBaseURL  string
	RootfsSnapshotID string
	SandboxID        string
}

// SnapshotExportResult describes the exported sandbox-snapshot image.
type SnapshotExportResult struct {
	BootIndexDigest string
	BootIndexTag    string
}

// ExportSnapshot exports a sandbox-snapshot image from an existing rootfs
// snapshot or sandbox.
func ExportSnapshot(ctx context.Context, req SnapshotExportRequest) (SnapshotExportResult, error) {
	_ = ctx
	_ = req
	return SnapshotExportResult{}, fmt.Errorf("snapshot export requires the native convert workflow")
}
