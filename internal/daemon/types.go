package daemon

import (
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/volume"
)

type pullImageRequest struct {
	ImageName string `json:"image_name"`
	PlainHTTP bool   `json:"plain_http,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
}

type pushImageRequest struct {
	LocalImage  string `json:"local_image"`
	RemoteImage string `json:"remote_image"`
	PlainHTTP   bool   `json:"plain_http,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
}

type listImageRequest struct {
	Filters []string `json:"filters,omitempty"`
}

type removeImageRequest struct {
	ImageName   string `json:"image_name"`
	Synchronous bool   `json:"synchronous,omitempty"`
}

type listImageResponse struct {
	Images []runtimeapi.ImageRecord `json:"images"`
}

type listSnapshotRequest struct {
	Filters []string `json:"filters,omitempty"`
}

type removeSnapshotRequest struct {
	Key string `json:"key"`
}

type snapshotInfoRequest struct {
	Key string `json:"key"`
}

type listSnapshotResponse struct {
	Snapshots []runtimeapi.SnapshotRecord `json:"snapshots"`
}

type removeSnapshotResponse struct {
	Status string `json:"status"`
}

type sandboxCreateRequest struct {
	TemplateID   string                           `json:"template_id"`
	VMMName      string                           `json:"vmm_name"`
	SandboxID    string                           `json:"sandbox_id"`
	LeaseID      string                           `json:"lease_id,omitempty"`
	VCPUNum      int64                            `json:"vcpu_num"`
	VCPUMax      int64                            `json:"vcpu_max"`
	RAMMB        int64                            `json:"ram_mb"`
	VolumeMounts []volume.Mount                   `json:"volumeMounts,omitempty"`
	Env          map[string]string                `json:"env,omitempty"`
	Network      *runtimeapi.SandboxNetworkConfig `json:"network,omitempty"`
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
	ConchInitVersion     string `json:"conchInitVersion"`
	Alias                string `json:"alias"`
	ConchInitAccessToken string `json:"conchInitAccessToken"`
	Domain               string `json:"domain"`
}

type sandboxInspectResponse struct {
	TemplateID       string                           `json:"templateID"`
	ImageName        string                           `json:"imageName"`
	SnapshotID       string                           `json:"snapshotID"`
	SandboxID        string                           `json:"sandboxID"`
	StartedAt        string                           `json:"startedAt"`
	EndAt            string                           `json:"endAt"`
	CPUCount         int64                            `json:"cpuCount"`
	MemoryMB         int64                            `json:"memoryMB"`
	DiskSizeMB       int64                            `json:"diskSizeMB"`
	ConchInitVersion string                           `json:"conchInitVersion"`
	Alias            string                           `json:"alias"`
	Domain           *string                          `json:"domain,omitempty"`
	Metadata         map[string]string                `json:"metadata"`
	Lifecycle        *sandboxLifecycleResponse        `json:"lifecycle,omitempty"`
	VolumeMounts     []sandboxVolumeMountResponse     `json:"volumeMounts"`
	Network          *runtimeapi.SandboxNetworkConfig `json:"network,omitempty"`
}

type sandboxLifecycleRequest struct {
	SandboxID string `json:"sandbox_id"`
}

type sandboxCheckpointRequest struct {
	SandboxID string            `json:"sandbox_id"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type templateListRequest struct {
	Origin   string `json:"origin,omitempty"`
	BootMode string `json:"boot_mode,omitempty"`
}

type templateIDRequest struct {
	ID string `json:"template_id"`
}

type templateCreateRequest struct {
	Source    string            `json:"source"`
	PlainHTTP bool              `json:"plain_http,omitempty"`
	Username  string            `json:"username,omitempty"`
	Password  string            `json:"password,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type templatePullRequest struct {
	Reference string            `json:"reference"`
	PlainHTTP bool              `json:"plain_http,omitempty"`
	Username  string            `json:"username,omitempty"`
	Password  string            `json:"password,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type templatePushRequest struct {
	TemplateID      string `json:"template_id"`
	RemoteReference string `json:"remote_reference"`
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
}

type templateUnpackRequest struct {
	TemplateID string `json:"template_id"`
}

type templateRecordResponse = runtimeapi.TemplateRecord

type templateListResponse struct {
	Items []templateRecordResponse `json:"items"`
}
