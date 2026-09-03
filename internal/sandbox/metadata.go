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
	StateUnknown   State = "UNKNOWN"
)

type SnapshotRef = snapshot.RuntimeSnapshotRef

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
