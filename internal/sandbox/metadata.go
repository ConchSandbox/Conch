package sandbox

import (
	"context"

	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/snapshot"
)

type State string

const (
	StateCreating  State = "CREATING"
	StateReady     State = "READY"
	StateSuspended State = "SUSPENDED"
	StatePaused    State = "PAUSED"
	StateUnknown   State = "UNKNOWN"
)

type SnapshotRef = snapshot.RuntimeSnapshotRef

type Record struct {
	ID                       string
	RuntimeID                string
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
	E2B                      bool
	EnvdVersion              string
	Metadata                 map[string]string
	ExpiresAt                int64
	TimeoutAction            string
	MaskRequestHost          string
	VolumeMounts             []VolumeMountRecord
}

// VolumeMountRecord is the persisted form of a named volume mount.
type VolumeMountRecord struct {
	Name string `json:"name"`
	Path string `json:"path"`
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
