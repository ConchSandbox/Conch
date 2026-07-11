package state

const (
	SandboxReady     = "READY"
	SandboxNotReady  = "NOTREADY"
	SandboxSuspended = "SUSPENDED"
	SandboxStopped   = "STOPPED"
	SandboxUnknown   = "UNKNOWN"

	NetworkSlotCreating = "CREATING"
	NetworkSlotWarmIdle = "WARM_IDLE"
	NetworkSlotAssigned = "ASSIGNED"
	NetworkSlotCleaning = "CLEANING"

	ContainerCreated = "CREATED"
	ContainerRunning = "RUNNING_PLACEHOLDER"
	ContainerExited  = "EXITED"
	ContainerUnknown = "UNKNOWN"

	TemplateOriginImage      = "image"
	TemplateOriginCheckpoint = "checkpoint"
	TemplateBootModeCold     = "cold"
	TemplateBootModeResume   = "resume"

	TemplateCreating = "CREATING"
	TemplateReady    = "READY"
	TemplateFailed   = "FAILED"
)

type SandboxRecord struct {
	PodSandboxID    string            `json:"pod_sandbox_id"`
	ConchSandboxID  string            `json:"conch_sandbox_id"`
	Namespace       string            `json:"namespace"`
	PodNamespace    string            `json:"pod_namespace,omitempty"`
	Name            string            `json:"name"`
	UID             string            `json:"uid"`
	Attempt         uint32            `json:"attempt"`
	State           string            `json:"state"`
	CreatedAt       int64             `json:"created_at"`
	StoppedAt       int64             `json:"stopped_at,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	RuntimeHandler  string            `json:"runtime_handler,omitempty"`
	LeaseID         string            `json:"lease_id,omitempty"`
	TemplateID      string            `json:"template_id,omitempty"`
	ImageName       string            `json:"image_name,omitempty"`
	SnapshotID      string            `json:"snapshot_id,omitempty"`
	IP              string            `json:"ip,omitempty"`
	VMMName         string            `json:"vmm_name,omitempty"`
	VCPUNum         int64             `json:"vcpu_num,omitempty"`
	RamMB           int64             `json:"ram_mb,omitempty"`
	VMMPID          int               `json:"vmm_pid,omitempty"`
	VMMSocketPath   string            `json:"vmm_socket_path,omitempty"`
	VsockCID        uint32            `json:"vsock_cid,omitempty"`
	VsockSocketPath string            `json:"vsock_socket_path,omitempty"`
	NetworkSlotKey  string            `json:"network_slot_key,omitempty"`
	NetworkNS       string            `json:"network_ns,omitempty"`
	RootfsKey       string            `json:"rootfs_key,omitempty"`
	MemKey          string            `json:"mem_key,omitempty"`
	ParentRootfsID  string            `json:"parent_rootfs_id,omitempty"`
	ParentMemID     string            `json:"parent_mem_id,omitempty"`
	ParentVMID      string            `json:"parent_vm_id,omitempty"`
	RootfsMount     string            `json:"rootfs_mount,omitempty"`
	MemMount        string            `json:"mem_mount,omitempty"`
	VMMount         string            `json:"vm_mount,omitempty"`
	SnapshotRootDir string            `json:"snapshot_root_dir,omitempty"`
	LastError       string            `json:"last_error,omitempty"`
}

type NetworkSlotRecord struct {
	SlotKey   string `json:"slot_key"`
	SlotIndex int    `json:"slot_index"`
	State     string `json:"state"`

	SandboxID string `json:"sandbox_id,omitempty"`
	NetNSPath string `json:"netns_path,omitempty"`
	CNIID     string `json:"cni_id,omitempty"`
	CNIIP     string `json:"cni_ip,omitempty"`

	UpdatedAt int64  `json:"updated_at,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

type ContainerRecord struct {
	ContainerID  string            `json:"container_id"`
	PodSandboxID string            `json:"pod_sandbox_id"`
	Name         string            `json:"name"`
	State        string            `json:"state"`
	CreatedAt    int64             `json:"created_at"`
	StartedAt    int64             `json:"started_at,omitempty"`
	FinishedAt   int64             `json:"finished_at,omitempty"`
	Image        string            `json:"image,omitempty"`
	ImageRef     string            `json:"image_ref,omitempty"`
	Command      []string          `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	LogPath      string            `json:"log_path,omitempty"`
	ExitCode     int32             `json:"exit_code,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	LastError    string            `json:"last_error,omitempty"`
}

type TemplateRecord struct {
	ID               string            `json:"id"`
	Origin           string            `json:"origin"`
	Namespace        string            `json:"namespace"`
	State            string            `json:"state"`
	RootfsKey        string            `json:"rootfs_key,omitempty"`
	MemKey           string            `json:"mem_key,omitempty"`
	VMKey            string            `json:"vm_key,omitempty"`
	ParentTemplateID string            `json:"parent_template_id,omitempty"`
	SourceSandboxID  string            `json:"source_sandbox_id,omitempty"`
	ImageName        string            `json:"image_name,omitempty"`
	BuildRef         string            `json:"build_ref,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	CreatedAt        int64             `json:"created_at"`
	UpdatedAt        int64             `json:"updated_at"`
	LastError        string            `json:"last_error,omitempty"`
}
