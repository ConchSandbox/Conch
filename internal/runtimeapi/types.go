package runtimeapi

import (
	"time"

	"github.com/openeuler/Conch/internal/volume"
)

type SandboxNetworkConfig struct {
	AllowOut            []string `json:"allowOut,omitempty"`
	DenyOut             []string `json:"denyOut,omitempty"`
	AllowIn             []string `json:"allowIn,omitempty"`
	DenyIn              []string `json:"denyIn,omitempty"`
	AllowInternetAccess *bool    `json:"allow_internet_access,omitempty"`
}

type SandboxNetworkUpdateOptions struct {
	SandboxID string
	Network   *SandboxNetworkConfig
}

// ImageRecord.Kind values exposed by the image API. These classify the
// user-visible image record, not the io.conch.kind annotation stored on Boot
// Index component descriptors.
const (
	ImageKindOCIImage             = "oci-image"
	ImageKindBootIndexCold        = "boot-index-cold"
	ImageKindBootIndexResume      = "boot-index-resume"
	ImageKindBootComponentRootfs  = "boot-component-rootfs"
	ImageKindBootComponentSandbox = "boot-component-sandbox"
	ImageKindBootComponentMemory  = "boot-component-memory"
)

type SandboxCreateOptions struct {
	SandboxID    string
	LeaseID      string
	TemplateID   string
	VMMName      string
	VCPUNum      int64
	VCPUMax      int64
	RamMB        int64
	VolumeMounts []volume.Mount
	Env          map[string]string
	Network      *SandboxNetworkConfig
}

type SandboxDefaults struct {
	TemplateID string
	VMMName    string
	VCPUNum    int64
	VCPUMax    int64
	RamMB      int64
}

// Sandbox admission limits are intentionally fixed and are not part of the
// operator configuration surface.
const (
	SandboxMaxVCPU  int64 = 64
	SandboxMaxRAMMB int64 = 256 * 1024
)

type SandboxCreateResult struct {
	SandboxID  string
	IP         string
	AgentToken string
	TemplateID string
	VCPUNum    int64
	RamMB      int64
	CreatedAt  int64
}

type SandboxCheckpointOptions struct {
	SandboxID string
	Labels    map[string]string
}

type SandboxCheckpointResult struct {
	TemplateID string
}

type TemplateCreateOptions struct {
	Source     string
	KernelPath string
	InitrdPath string
	PlainHTTP  bool
	Username   string
	Password   string
	Labels     map[string]string
}

type TemplateCreateResult struct {
	TemplateID string
	BuildRef   string
}

type TemplatePullOptions struct {
	Reference string
	PlainHTTP bool
	Username  string
	Password  string
	Labels    map[string]string
}

type TemplatePullResult struct {
	TemplateID string
	BuildRef   string
}

type TemplatePushOptions struct {
	TemplateID      string
	RemoteReference string
	PlainHTTP       bool
	Username        string
	Password        string
}

type TemplateUnpackOptions struct {
	TemplateID string
}

type TemplateListOptions struct {
	Origin   string
	BootMode string
}

type TemplateRecord struct {
	TemplateID       string            `json:"template_id"`
	Origin           string            `json:"origin"`
	BootMode         string            `json:"boot_mode"`
	ParentTemplateID string            `json:"parent_template_id,omitempty"`
	SourceSandboxID  string            `json:"source_sandbox_id,omitempty"`
	SourceRef        string            `json:"source_ref,omitempty"`
	BuildRef         string            `json:"build_ref"`
	Labels           map[string]string `json:"labels,omitempty"`
	CreatedAt        int64             `json:"created_at,omitempty"`
}

type PullImageOptions struct {
	ImageName string
	PlainHTTP bool
	Username  string
	Password  string
}

type PushImageOptions struct {
	LocalImage  string
	RemoteImage string
	PlainHTTP   bool
	Username    string
	Password    string
}

type ListImagesOptions struct {
	Filters []string
}

type RemoveImageOptions struct {
	ImageName   string
	Synchronous bool
}

type ImageRecord struct {
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

type ListSnapshotsOptions struct {
	Filters []string
}

type RemoveSnapshotOptions struct {
	Key string
}

type SnapshotInfoOptions struct {
	Key string
}

type SnapshotRecord struct {
	Key         string            `json:"key"`
	Kind        string            `json:"kind,omitempty"`
	Parent      string            `json:"parent,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	StoragePath string            `json:"storage_path,omitempty"`
	CreatedAt   time.Time         `json:"created_at,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at,omitempty"`
}
