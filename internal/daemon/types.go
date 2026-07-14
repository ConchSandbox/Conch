package daemon

import (
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

type pullImageRequest struct {
	ImageName string `json:"image_name"`
	Namespace string `json:"namespace,omitempty"`
	PlainHTTP bool   `json:"plain_http,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
}

type pushImageRequest struct {
	LocalImage      string `json:"local_image"`
	RemoteImage     string `json:"remote_image"`
	Namespace       string `json:"namespace,omitempty"`
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	RegistryTimeout string `json:"registry_timeout,omitempty"`
}

type listImageRequest struct {
	Namespace string   `json:"namespace,omitempty"`
	Filters   []string `json:"filters,omitempty"`
}

type removeImageRequest struct {
	Namespace   string `json:"namespace,omitempty"`
	ImageName   string `json:"image_name"`
	Synchronous bool   `json:"synchronous,omitempty"`
}

type unpackImageRequest struct {
	ImageName string `json:"image_name"`
	Namespace string `json:"namespace,omitempty"`
}

type imageRecordResponse struct {
	Name            string            `json:"name"`
	TargetDigest    string            `json:"target_digest"`
	RepoDigests     []string          `json:"repo_digests,omitempty"`
	TargetMediaType string            `json:"target_media_type"`
	Size            int64             `json:"size,omitempty"`
	Kind            string            `json:"kind,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	CreatedAt       time.Time         `json:"created_at,omitempty"`
	UpdatedAt       time.Time         `json:"updated_at,omitempty"`
}

type listImageResponse struct {
	Images []imageRecordResponse `json:"images"`
}

type importImageArchiveResponse struct {
	SnapshotKey string `json:"snapshot_key"`
	ImageName   string `json:"image_name"`
}

type convertImageRequest struct {
	Source       string `json:"source"`
	Namespace    string `json:"namespace,omitempty"`
	BootIndexTag string `json:"boot_index_tag"`
	PlainHTTP    bool   `json:"plain_http,omitempty"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	Snapshot     bool   `json:"snapshot,omitempty"`
}

type convertImageResponse struct {
	BootIndexDigest string `json:"boot_index_digest"`
	BootIndexTag    string `json:"boot_index_tag"`
	RootfsImageRef  string `json:"rootfs_image_ref,omitempty"`
	SourceImageRef  string `json:"source_image_ref,omitempty"`
}

type snapshotExportRequest struct {
	Namespace        string `json:"namespace,omitempty"`
	BootIndexTag     string `json:"boot_index_tag"`
	RootfsSnapshotID string `json:"snapshot_id,omitempty"`
	SandboxID        string `json:"sandbox_id,omitempty"`
}

type snapshotExportResponse struct {
	BootIndexDigest string `json:"boot_index_digest"`
	BootIndexTag    string `json:"boot_index_tag"`
}

type snapshotInfoRequest struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
}

type listSandboxRequest struct {
	Namespace string `json:"namespace,omitempty"`
}

type sandboxSummaryResponse struct {
	SandboxID    string `json:"sandbox_id"`
	PodSandboxID string `json:"pod_sandbox_id,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
	State        string `json:"state,omitempty"`
	Name         string `json:"name,omitempty"`
	UID          string `json:"uid,omitempty"`
	ImageName    string `json:"image_name,omitempty"`
	SnapshotID   string `json:"snapshot_id,omitempty"`
	IP           string `json:"ip,omitempty"`
	VMMName      string `json:"vmm_name,omitempty"`
	VCPUNum      int64  `json:"vcpu_num,omitempty"`
	RamMB        int64  `json:"ram_mb,omitempty"`
	CreatedAt    int64  `json:"created_at,omitempty"`
	StoppedAt    int64  `json:"stopped_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

type listSandboxResponse struct {
	Sandboxes []sandboxSummaryResponse `json:"sandboxes"`
}

type getSandboxResponse struct {
	Sandbox sandboxSummaryResponse `json:"sandbox"`
}

func imageRecordResponses(records []runtimeapi.ImageRecord) []imageRecordResponse {
	out := make([]imageRecordResponse, 0, len(records))
	for _, record := range records {
		out = append(out, imageRecordResponse{
			Name:            record.Name,
			TargetDigest:    record.TargetDigest,
			RepoDigests:     append([]string(nil), record.RepoDigests...),
			TargetMediaType: record.TargetMediaType,
			Size:            record.Size,
			Kind:            record.Kind,
			Labels:          copyStringMap(record.Labels),
			CreatedAt:       record.CreatedAt,
			UpdatedAt:       record.UpdatedAt,
		})
	}
	return out
}

type sandboxLogEntryResponse struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandbox_id"`
	Timestamp int64  `json:"timestamp"`
	Level     string `json:"level"`
	Event     string `json:"event"`
	Message   string `json:"message"`
}

type getSandboxLogsResponse struct {
	Logs       []sandboxLogEntryResponse `json:"logs"`
	NextCursor string                    `json:"next_cursor,omitempty"`
	HasMore    bool                      `json:"has_more"`
}

func importImageArchiveHTTPResponse(result runtimeapi.ImportImageArchiveResult) importImageArchiveResponse {
	return importImageArchiveResponse{
		SnapshotKey: result.SnapshotKey,
		ImageName:   result.ImageName,
	}
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

type updateSandboxNetworkRequest struct {
	AllowOut           *bool    `json:"allowOut,omitempty"`
	DenyOut            []string `json:"denyOut,omitempty"`
	EgressProxy        string   `json:"egressProxy,omitempty"`
	AllowPublicTraffic *bool    `json:"allowPublicTraffic,omitempty"`
}

type sandboxNetworkPolicyResponse struct {
	SandboxID          string   `json:"sandbox_id"`
	AllowOut           *bool    `json:"allowOut,omitempty"`
	DenyOut            []string `json:"denyOut,omitempty"`
	EgressProxy        string   `json:"egressProxy,omitempty"`
	AllowPublicTraffic *bool    `json:"allowPublicTraffic,omitempty"`
}

type updateSandboxNetworkResponse struct {
	Status    string                       `json:"status"`
	SandboxID string                       `json:"sandbox_id"`
	Network   sandboxNetworkPolicyResponse `json:"network"`
}