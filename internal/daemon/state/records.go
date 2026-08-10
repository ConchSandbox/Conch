package state

import "github.com/openeuler/Conch/internal/runtimeapi"

const (
	SandboxReady     = "READY"
	SandboxSuspended = "SUSPENDED"
	SandboxUnknown   = "UNKNOWN"
)

type SandboxRecord struct {
	SandboxID                     string                           `json:"sandbox_id"`
	State                         string                           `json:"state"`
	CreatedAt                     int64                            `json:"created_at"`
	SourceTemplateID              string                           `json:"source_template_id,omitempty"`
	CheckpointHeadTemplateID      string                           `json:"checkpoint_head_template_id"`
	CheckpointHeadBootIndexDigest string                           `json:"checkpoint_head_boot_index_digest"`
	IP                            string                           `json:"ip,omitempty"`
	VCPUNum                       int64                            `json:"vcpu_num,omitempty"`
	RamMB                         int64                            `json:"ram_mb,omitempty"`
	Network                       *runtimeapi.SandboxNetworkConfig `json:"network,omitempty"`
	LastError                     string                           `json:"last_error,omitempty"`
}
