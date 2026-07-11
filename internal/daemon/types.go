package daemon

import (
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

type pullImageRequest struct {
	ImageName  string `json:"image_name"`
	Namespace  string `json:"namespace,omitempty"`
	PlainHTTP  bool   `json:"plain_http,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	SkipUnpack bool   `json:"skip_unpack,omitempty"`
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

type snapshotInfoRequest struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
}

type sandboxLifecycleRequest struct {
	Namespace string `json:"namespace,omitempty"`
	SandboxID string `json:"sandbox_id"`
}

type sandboxCheckpointRequest struct {
	Namespace string            `json:"namespace,omitempty"`
	SandboxID string            `json:"sandbox_id"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type templateListRequest struct {
	Namespace string `json:"namespace,omitempty"`
	Origin    string `json:"origin,omitempty"`
	BootMode  string `json:"boot_mode,omitempty"`
}

type templateIDRequest struct {
	ID string `json:"id"`
}

type templateCreateRequest struct {
	Source       string            `json:"source"`
	Namespace    string            `json:"namespace,omitempty"`
	BootIndexTag string            `json:"boot_index_tag,omitempty"`
	PlainHTTP    bool              `json:"plain_http,omitempty"`
	Username     string            `json:"username,omitempty"`
	Password     string            `json:"password,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

type templateRecordResponse = runtimeapi.TemplateRecord

type templateListResponse struct {
	Items []templateRecordResponse `json:"items"`
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
