package daemon

import (
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/volume"
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

type listSnapshotRequest struct {
	Namespace string   `json:"namespace,omitempty"`
	Filters   []string `json:"filters,omitempty"`
}

type removeSnapshotRequest struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
}

type snapshotInfoRequest struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
}

type snapshotRecordResponse struct {
	Key         string            `json:"key"`
	Kind        string            `json:"kind,omitempty"`
	Parent      string            `json:"parent,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	StoragePath string            `json:"storage_path,omitempty"`
	CreatedAt   time.Time         `json:"created_at,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at,omitempty"`
}

type listSnapshotResponse struct {
	Snapshots []snapshotRecordResponse `json:"snapshots"`
}

type removeSnapshotResponse struct {
	Status string `json:"status"`
}

type sandboxCreateRequest struct {
	Namespace    string            `json:"namespace"`
	TemplateID   string            `json:"template_id"`
	VMMName      string            `json:"vmm_name"`
	SandboxID    string            `json:"sandbox_id"`
	LeaseID      string            `json:"lease_id,omitempty"`
	VCPUNum      int64             `json:"vcpu_num"`
	VCPUMax      int64             `json:"vcpu_max"`
	RAMMB        int64             `json:"ram_mb"`
	VolumeMounts []volume.Mount    `json:"volumeMounts,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

type sandboxVolumeMountResponse struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type sandboxLifecycleResponse struct {
	AutoResume bool `json:"autoResume"`
}

type createSandboxResponse struct {
	TemplateID           string `json:"templateID"`
	SandboxID            string `json:"sandboxID"`
	Namespace            string `json:"namespace"`
	ConchInitVersion     string `json:"conchInitVersion"`
	Alias                string `json:"alias"`
	ConchInitAccessToken string `json:"conchInitAccessToken"`
	Domain               string `json:"domain"`
}

type sandboxInspectResponse struct {
	TemplateID       string                       `json:"templateID"`
	ImageName        string                       `json:"imageName"`
	SnapshotID       string                       `json:"snapshotID"`
	SandboxID        string                       `json:"sandboxID"`
	Namespace        string                       `json:"namespace"`
	StartedAt        string                       `json:"startedAt"`
	EndAt            string                       `json:"endAt"`
	CPUCount         int64                        `json:"cpuCount"`
	MemoryMB         int64                        `json:"memoryMB"`
	DiskSizeMB       int64                        `json:"diskSizeMB"`
	ConchInitVersion string                       `json:"conchInitVersion"`
	Alias            string                       `json:"alias"`
	Domain           *string                      `json:"domain,omitempty"`
	Metadata         map[string]string            `json:"metadata"`
	Lifecycle        *sandboxLifecycleResponse    `json:"lifecycle,omitempty"`
	VolumeMounts     []sandboxVolumeMountResponse `json:"volumeMounts"`
}

type sandboxLogEntryResponse struct {
	Timestamp string            `json:"timestamp"`
	Message   string            `json:"message"`
	Level     string            `json:"level"`
	Fields    map[string]string `json:"fields"`
}

type getSandboxLogsResponse struct {
	Logs []sandboxLogEntryResponse `json:"logs"`
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

type templatePullRequest struct {
	Reference string            `json:"reference"`
	Namespace string            `json:"namespace,omitempty"`
	PlainHTTP bool              `json:"plain_http,omitempty"`
	Username  string            `json:"username,omitempty"`
	Password  string            `json:"password,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type templatePushRequest struct {
	TemplateID      string `json:"template_id"`
	RemoteReference string `json:"remote_reference"`
	Namespace       string `json:"namespace,omitempty"`
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	RegistryTimeout string `json:"registry_timeout,omitempty"`
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

func snapshotRecordHTTPResponse(record runtimeapi.SnapshotRecord) snapshotRecordResponse {
	return snapshotRecordResponse{
		Key:         record.Key,
		Kind:        record.Kind,
		Parent:      record.Parent,
		Labels:      copyStringMap(record.Labels),
		StoragePath: record.StoragePath,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
	}
}

func snapshotRecordHTTPResponses(records []runtimeapi.SnapshotRecord) []snapshotRecordResponse {
	if records == nil {
		return nil
	}
	out := make([]snapshotRecordResponse, 0, len(records))
	for _, record := range records {
		out = append(out, snapshotRecordHTTPResponse(record))
	}
	return out
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
