package runtimeapi

import (
	"time"

	"github.com/openeuler/Conch/internal/volume"
)

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
}

type SandboxDefaults struct {
	TemplateID string
	VMMName    string
	VCPUNum    int64
	VCPUMax    int64
	RamMB      int64
}

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
	TemplateID      string
	BootIndexDigest string
}

type TemplateCreateOptions struct {
	Source       string
	KernelPath   string
	InitrdPath   string
	BootIndexTag string
	PlainHTTP    bool
	Username     string
	Password     string
	Labels       map[string]string
}

type TemplateCreateResult struct {
	TemplateID      string
	BootIndexDigest string
	BootIndexTag    string
}

type TemplatePullOptions struct {
	Reference string
	PlainHTTP bool
	Username  string
	Password  string
	Labels    map[string]string
}

type TemplatePullResult struct {
	TemplateID      string
	BootIndexDigest string
	BuildRef        string
}

type TemplatePushOptions struct {
	TemplateID      string
	RemoteReference string
	PlainHTTP       bool
	Username        string
	Password        string
}

type TemplateListOptions struct {
	Origin   string
	BootMode string
}

type TemplateRecord struct {
	ID               string            `json:"id"`
	Origin           string            `json:"origin"`
	BootMode         string            `json:"boot_mode"`
	BootIndexDigest  string            `json:"boot_index_digest,omitempty"`
	ParentTemplateID string            `json:"parent_template_id,omitempty"`
	SourceSandboxID  string            `json:"source_sandbox_id,omitempty"`
	ImageName        string            `json:"image_name,omitempty"`
	BuildRef         string            `json:"build_ref,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	CreatedAt        int64             `json:"created_at,omitempty"`
}

type PullImageOptions struct {
	ImageName  string
	PlainHTTP  bool
	Username   string
	Password   string
	SkipUnpack bool
}

type PullImageResult struct {
	Refs map[string]string
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

type UnpackImageOptions struct {
	ImageName string
}

type ImportImageArchiveOptions struct {
	ImportedTag string
}

type ImportImageArchiveResult struct {
	SnapshotKey string
	ImageName   string
}

type ExportImageArchiveOptions struct {
	ImageName string
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
