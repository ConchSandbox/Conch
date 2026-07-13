package conchruntime

import (
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

type SandboxCreateOptions = runtimeapi.SandboxCreateOptions
type SandboxCreateResult = runtimeapi.SandboxCreateResult
type SandboxDefaults = runtimeapi.SandboxDefaults
type ContainerCreateOptions = runtimeapi.ContainerCreateOptions
type ContainerCreateResult = runtimeapi.ContainerCreateResult
type PullImageOptions = runtimeapi.PullImageOptions
type PullImageResult = runtimeapi.PullImageResult

type SandboxLogEntry struct {
	ID        uint64
	Time      time.Time
	SandboxID string
	Level     string
	Message   string
}

type SandboxLogsOptions struct {
	SandboxID string
	Cursor    uint64
	Limit     int
	Level     string
	Search    string
}

type SandboxLogsResult struct {
	Logs       []SandboxLogEntry
	NextCursor uint64
}
