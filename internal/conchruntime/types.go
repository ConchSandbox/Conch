package conchruntime

import (
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

type SandboxCreateOptions = runtimeapi.SandboxCreateOptions
type SandboxCreateResult = runtimeapi.SandboxCreateResult
type SandboxDefaults = runtimeapi.SandboxDefaults
type SandboxNetworkUpdateOptions = runtimeapi.SandboxNetworkUpdateOptions
type SandboxLifecycleOptions = runtimeapi.SandboxLifecycleOptions
type SandboxCheckpointOptions = runtimeapi.SandboxCheckpointOptions
type SandboxCheckpointResult = runtimeapi.SandboxCheckpointResult
type TemplateCreateOptions = runtimeapi.TemplateCreateOptions
type TemplateCreateResult = runtimeapi.TemplateCreateResult
type TemplatePullOptions = runtimeapi.TemplatePullOptions
type TemplatePullResult = runtimeapi.TemplatePullResult
type TemplatePushOptions = runtimeapi.TemplatePushOptions
type ContainerCreateOptions = runtimeapi.ContainerCreateOptions
type ContainerCreateResult = runtimeapi.ContainerCreateResult
type PullImageOptions = runtimeapi.PullImageOptions
type PullImageResult = runtimeapi.PullImageResult

type SandboxLogEntry struct {
	ID        uint64
	Time      time.Time
	Namespace string
	SandboxID string
	Level     string
	Message   string
}

type SandboxLogKey struct {
	Namespace string
	SandboxID string
}

type SandboxLogsOptions struct {
	Namespace string
	SandboxID string
	Cursor    string
	Limit     int
	Direction string
	Level     string
	Search    string
}

type SandboxLogsResult struct {
	Logs       []SandboxLogEntry
	NextCursor string
}
