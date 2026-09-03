package daemon

import (
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/volume"
)

// webhookCreateRequest is the daemon HTTP API payload for registering a Webhook.
type webhookCreateRequest struct {
	Name   string   `json:"name"`
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

type webhookResponse struct {
	WebhookID string   `json:"webhook_id"`
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	CreatedAt string   `json:"createdAt"`
}

type listWebhooksResponse struct {
	Webhooks []webhookResponse `json:"webhooks"`
}

type deleteWebhookResponse struct {
	WebhookID string `json:"webhook_id"`
	Status    string `json:"status"`
}

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
	TemplateName string                           `json:"template_name"`
	TemplateID   string                           `json:"template_id"`
	VMMName      string                           `json:"vmm_name"`
	SandboxID    string                           `json:"sandbox_id"`
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
	TemplateName         string `json:"templateName"`
	TemplateID           string `json:"templateID"`
	SandboxID            string `json:"sandboxID"`
	ConchInitVersion     string `json:"conchInitVersion"`
	Alias                string `json:"alias"`
	ConchInitAccessToken string `json:"conchInitAccessToken"`
	Domain               string `json:"domain"`
}

type sandboxInspectResponse struct {
	TemplateName     string                           `json:"templateName"`
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
	SandboxID    string            `json:"sandbox_id"`
	TemplateName string            `json:"template_name"`
	Labels       map[string]string `json:"labels,omitempty"`
}

type templateListRequest struct {
	Origin   string `json:"origin,omitempty"`
	BootMode string `json:"boot_mode,omitempty"`
}

type templateNameRequest struct {
	Name string `json:"name"`
}

type templateCreateRequest struct {
	Name      string            `json:"name"`
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
	Name            string `json:"name"`
	RemoteReference string `json:"remote_reference"`
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
}

type templateUnpackRequest struct {
	Name string `json:"name"`
}

type templateRecordResponse = runtimeapi.TemplateRecord

type templateListResponse struct {
	Items []templateRecordResponse `json:"items"`
}
