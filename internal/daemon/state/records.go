package state

import "github.com/openeuler/Conch/internal/template"

const (
	SandboxReady     = "READY"
	SandboxNotReady  = "NOTREADY"
	SandboxSuspended = "SUSPENDED"
	SandboxUnknown   = "UNKNOWN"

	NetworkSlotTransient = "TRANSIENT"
	NetworkSlotIdle      = "IDLE"
	NetworkSlotAssigned  = "ASSIGNED"
)

type SandboxRecord struct {
	SandboxID string `json:"sandbox_id"`
	Namespace string `json:"namespace"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
	LeaseID   string `json:"lease_id,omitempty"`
	// SourceTemplateID and SourceBootIndexDigest identify the immutable boot
	// content used to create this sandbox.
	SourceTemplateID              string         `json:"source_template_id,omitempty"`
	SourceBootIndexDigest         string         `json:"source_boot_index_digest,omitempty"`
	CheckpointHeadTemplateID      string         `json:"checkpoint_head_template_id,omitempty"`
	CheckpointHeadBootIndexDigest string         `json:"checkpoint_head_boot_index_digest,omitempty"`
	IP                            string         `json:"ip,omitempty"`
	VMMName                       string         `json:"vmm_name,omitempty"`
	VCPUNum                       int64          `json:"vcpu_num,omitempty"`
	RamMB                         int64          `json:"ram_mb,omitempty"`
	VMMPID                        int            `json:"vmm_pid,omitempty"`
	VMMSocketPath                 string         `json:"vmm_socket_path,omitempty"`
	VsockCID                      uint32         `json:"vsock_cid,omitempty"`
	VsockSocketPath               string         `json:"vsock_socket_path,omitempty"`
	NetworkSlotID                 int            `json:"network_slot_id,omitempty"`
	RootfsKey                     string         `json:"rootfs_key,omitempty"`
	MemKey                        string         `json:"mem_key,omitempty"`
	RootfsMount                   string         `json:"rootfs_mount,omitempty"`
	RootfsPmemPaths               []string       `json:"rootfs_pmem_paths,omitempty"`
	MemMount                      string         `json:"mem_mount,omitempty"`
	VMMount                       string         `json:"vm_mount,omitempty"`
	SnapshotRootDir               string         `json:"snapshot_root_dir,omitempty"`
	LastError                     string         `json:"last_error,omitempty"`
	VolumeDevices                 []VolumeDevice `json:"volume_devices,omitempty"`
}

type VolumeDevice struct {
	SandboxID  string `json:"sandbox_id,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Backend    string `json:"backend,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Socket     string `json:"socket,omitempty"`
	VolumeDir  string `json:"volume_dir,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	PID        int    `json:"pid,omitempty"`
	StartTime  uint64 `json:"start_time,omitempty"`
}

type NetworkSlotRecord struct {
	// SlotID is the sole slot identity. Storage keys and OS resource names are
	// derived from it.
	SlotID int    `json:"slot_id"`
	State  string `json:"state"`

	SandboxID string `json:"sandbox_id,omitempty"`
	CNIIP     string `json:"cni_ip,omitempty"`

	UpdatedAt int64  `json:"updated_at,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// CheckpointPublication describes the single transaction that creates a
// complete checkpoint Template Entry and advances the source Sandbox's
// logical checkpoint head.
type CheckpointPublication struct {
	Entry                       template.Entry
	SandboxID                   string
	ExpectedHeadTemplateID      string
	ExpectedHeadBootIndexDigest string
}
