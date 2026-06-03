package runtimeapi

type SandboxCreateOptions struct {
	Namespace      string
	PodSandboxID   string
	SandboxID      string
	Name           string
	UID            string
	Attempt        uint32
	Labels         map[string]string
	Annotations    map[string]string
	RuntimeHandler string
	LeaseID        string
	ImageName      string
	SnapshotID     string
	UseSnapshot    bool
	VMMName        string
	VCPUNum        int64
	VCPUMax        int64
	RamMB          int64
}

type SandboxDefaults struct {
	ImageName string
	VMMName   string
	VCPUNum   int64
	VCPUMax   int64
	RamMB     int64
}

type SandboxCreateResult struct {
	PodSandboxID string
	SandboxID    string
	Namespace    string
	IP           string
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
	ImageName          string
	Namespace          string
	Username           string
	Password           string
	DefaultKernelImage string
}

type PullImageResult struct {
	Refs map[string]string
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

type ImageRecord struct {
	Name            string
	TargetDigest    string
	RepoDigests     []string
	TargetMediaType string
	Size            int64
	Kind            string
	Labels          map[string]string
}

type ListSnapshotsOptions struct {
	Namespace string
	Filters   []string
}

type RemoveSnapshotOptions struct {
	Namespace string
	Key       string
}
