package runtimeapi

import "time"

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
	Namespace      string
	PodNamespace   string
	PodSandboxID   string
	SandboxID      string
	Name           string
	UID            string
	Attempt        uint32
	Labels         map[string]string
	Annotations    map[string]string
	RuntimeHandler string
	LeaseID        string
	TemplateID     string
	VMMName        string
	VCPUNum        int64
	VCPUMax        int64
	RamMB          int64
}

type SandboxDefaults struct {
	TemplateID string
	VMMName    string
	VCPUNum    int64
	VCPUMax    int64
	RamMB      int64
}

type SandboxCreateResult struct {
	PodSandboxID string
	SandboxID    string
	Namespace    string
	IP           string
	AgentToken   string
}

type SandboxLifecycleOptions struct {
	Namespace    string
	PodSandboxID string
}

type SandboxCheckpointOptions struct {
	Namespace    string
	PodSandboxID string
	Labels       map[string]string
}

type SandboxCheckpointResult struct {
	TemplateID      string
	BootIndexDigest string
}

type TemplateCreateOptions struct {
	Namespace    string
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
	Namespace string
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
	Namespace       string
	PlainHTTP       bool
	Username        string
	Password        string
	RegistryTimeout string
}

type TemplateListOptions struct {
	Namespace string
	Origin    string
	BootMode  string
}

type TemplateRecord struct {
	ID               string            `json:"id"`
	Origin           string            `json:"origin"`
	BootMode         string            `json:"boot_mode"`
	BootIndexDigest  string            `json:"boot_index_digest,omitempty"`
	Namespace        string            `json:"namespace"`
	State            string            `json:"state"`
	ParentTemplateID string            `json:"parent_template_id,omitempty"`
	SourceSandboxID  string            `json:"source_sandbox_id,omitempty"`
	ImageName        string            `json:"image_name,omitempty"`
	BuildRef         string            `json:"build_ref,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	CreatedAt        int64             `json:"created_at,omitempty"`
	UpdatedAt        int64             `json:"updated_at,omitempty"`
	LastError        string            `json:"last_error,omitempty"`
}

type ContainerCreateOptions struct {
	ContainerID  string
	PodSandboxID string
	Name         string
	Image        string
	ImageRef     string
	Command      []string
	Args         []string
	LogPath      string
	Labels       map[string]string
	Annotations  map[string]string
}

type ContainerCreateResult struct {
	ContainerID string
}

type PullImageOptions struct {
	ImageName  string
	Namespace  string
	PlainHTTP  bool
	Username   string
	Password   string
	SkipUnpack bool
}

type PullImageResult struct {
	Refs map[string]string
}

type PushImageOptions struct {
	LocalImage      string
	RemoteImage     string
	Namespace       string
	PlainHTTP       bool
	Username        string
	Password        string
	RegistryTimeout string
}

type ListImagesOptions struct {
	Namespace string
	Filters   []string
}

type RemoveImageOptions struct {
	Namespace   string
	ImageName   string
	Synchronous bool
}

type UnpackImageOptions struct {
	ImageName string
	Namespace string
}

type ImportImageArchiveOptions struct {
	Namespace   string
	ImportedTag string
}

type ImportImageArchiveResult struct {
	SnapshotKey string
	ImageName   string
}

type ExportImageArchiveOptions struct {
	Namespace string
	ImageName string
}

type ImageRecord struct {
	Name            string
	TargetDigest    string
	RepoDigests     []string
	TargetMediaType string
	Size            int64
	Kind            string
	Labels          map[string]string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type ListSnapshotsOptions struct {
	Namespace string
	Filters   []string
}

type RemoveSnapshotOptions struct {
	Namespace string
	Key       string
}
