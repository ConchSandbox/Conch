package sandbox

import (
	"context"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

type State string

const (
	StateCreating  State = "CREATING"
	StateReady     State = "READY"
	StateSuspended State = "SUSPENDED"
	StateUnknown   State = "UNKNOWN"
)

type SnapshotRef struct {
	Snapshotter string `json:"snapshotter"`
	Role        string `json:"role"`
	Key         string `json:"key"`
}

type Record struct {
	ID                       string
	VMMPID                   int
	State                    State
	CreatedAt                int64
	SourceTemplateName       string
	SourceTemplateID         string
	CheckpointHeadTemplateID string
	IP                       string
	VCPUNum                  int64
	RamMB                    int64
	Network                  *runtimeapi.SandboxNetworkConfig
	LastError                string
	RuntimeSnapshots         []SnapshotRef
}

type Filter struct {
	State State
}

type Store interface {
	Create(context.Context, Record) (Record, error)
	Update(context.Context, Record) (Record, error)
	Get(context.Context, string) (Record, error)
	List(context.Context, Filter) ([]Record, error)
	Delete(context.Context, string) error
}
