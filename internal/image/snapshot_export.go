package image

import (
	"context"
	"os"

	"github.com/openeuler/Conch/internal/image/conchconvert"
)

// SnapshotExportRequest is the unified input for exporting a snapshot-backed
// sandbox image from an existing rootfs snapshot or sandbox.
type SnapshotExportRequest struct {
	BootIndexTag     string
	ConfigPath       string
	ConchAPIBaseURL  string
	Namespace        string
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
	apiBase := req.ConchAPIBaseURL
	if apiBase == "" {
		apiBase = os.Getenv("CONCH_API_URL")
	}

	res, err := conchconvert.ExportSnapshot(ctx, conchconvert.SnapshotExportOpts{
		BootIndexTag:     req.BootIndexTag,
		ConfigPath:       req.ConfigPath,
		ConchAPIBaseURL:  apiBase,
		Namespace:        req.Namespace,
		RootfsSnapshotID: req.RootfsSnapshotID,
		SandboxID:        req.SandboxID,
	})
	if err != nil {
		return SnapshotExportResult{}, err
	}

	return SnapshotExportResult{
		BootIndexDigest: res.BootIndexDigest,
		BootIndexTag:    res.BootIndexTag,
	}, nil
}
