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

// Timeout action applied when an E2B sandbox expires. The zero value keeps the
// default behavior of deleting the sandbox; TimeoutActionPause runs the pause
// chain instead so the sandbox can be resumed with connect.
const (
	TimeoutActionPause = "pause"
)

type SnapshotRef = snapshot.RuntimeSnapshotRef

type Record struct {
	ID string
	// RuntimeID is the immutable identity of one runtime allocation. It
	// outlives entry removal and survives reuse of the public sandbox ID.
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
	// E2B marks sandboxes created through the E2B Node API.
	E2B bool
	// EnvdVersion records the envd build that bootstrapped the sandbox.
	EnvdVersion string
	// Metadata preserves caller-defined E2B sandbox metadata.
	Metadata map[string]string
	// ExpiresAt is the E2B TTL deadline (unix nanoseconds); zero never expires.
	ExpiresAt int64
	// Env preserves the E2B envVars so a paused sandbox can replay them on resume.
	Env map[string]string
	// TimeoutAction selects the expiry behavior for E2B sandboxes ("" deletes, "pause" pauses).
	TimeoutAction string
	// MaskRequestHost preserves the E2B network host mask applied by the sandbox proxy.
	MaskRequestHost string
	// VolumeMounts records the named volume mounts used by the sandbox.
	VolumeMounts []VolumeMountRecord
	// PauseTemplateName names the resume template created by the most recent pause,
	// so resume/kill can clean it up.
	PauseTemplateName string
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
