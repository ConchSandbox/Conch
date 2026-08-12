package volume

import (
	"regexp"
	"time"
)

// ProcessObservation is the immutable result published by the single
// virtiofsd monitor. Exited reports whether the handle proved process exit;
// Cause is diagnostic and must not be used to classify sandbox lifecycle.
type ProcessObservation struct {
	PID        int
	StartTime  uint64
	Exited     bool
	Cause      error
	ExitCode   *int
	Signal     string
	ObservedAt time.Time
}

// ProcessWatch broadcasts monitor completion by closing Done. Result is valid
// after Done closes. Multiple callers may wait without competing for a value.
type ProcessWatch struct {
	process *virtiofsProcess
}

func (w *ProcessWatch) Done() <-chan struct{} {
	if w == nil || w.process == nil {
		return nil
	}
	return w.process.Done()
}

func (w *ProcessWatch) Result() (ProcessObservation, bool) {
	if w == nil || w.process == nil {
		return ProcessObservation{}, false
	}
	return w.process.Result()
}

// Mount is a single user-declared volume mount: the host Source directory is
// bind-mounted into the sandbox shared dir and exposed to the guest at Path.
type Mount struct {
	Source   string `json:"source"`
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

type Config struct {
	MaxMounts int
	Backend   string
	Virtiofs  VirtiofsConfig
}

type VirtiofsConfig struct {
	Binary     string
	RuntimeDir string
}

// Device describes the single per-sandbox virtiofs device. A sandbox has 0 or
// 1 device; the slice plumbing keeps a 0-or-1 element slice for compatibility
// with state persistence and the sandbox manager.
type Device struct {
	SandboxID  string `json:"sandbox_id,omitempty"`
	Backend    string `json:"backend,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Socket     string `json:"socket,omitempty"`
	VolumeDir  string `json:"volume_dir,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	PID        int    `json:"pid,omitempty"`
	StartTime  uint64 `json:"start_time,omitempty"`
}

type PrepareRequest struct {
	SandboxID string
	Mounts    []Mount
}

// PreparedSandbox carries both the persistable device description and the
// exact in-memory process owner needed for active cleanup.
type PreparedSandbox struct {
	Devices []Device
	Watch   *ProcessWatch
}

type Backend interface {
	Name() string
	Prepare(req PrepareRequest) (PreparedSandbox, error)
	Cleanup(sandboxID string, prepared PreparedSandbox) error
	CleanupStaleResources() error
}

var safeSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func ValidateSegment(value, field string) error {
	if !safeSegmentRE.MatchString(value) {
		return &ValidationError{Field: field, Value: value, Pattern: safeSegmentRE.String()}
	}
	return nil
}

type ValidationError struct {
	Field   string
	Value   string
	Pattern string
}

func (e *ValidationError) Error() string {
	return e.Field + " must match " + e.Pattern + " (got " + e.Value + ")"
}
