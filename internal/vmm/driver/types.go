package driver

import "context"

type ResourceArgs struct {
	// CPU
	CPUBoot int64
	CPUMax  int64

	// Memory
	MemorySize int64
	MemoryPath string

	// Net
	NamespaceID string
	TapName     string

	// Kernel
	KernelPath string

	// Rootfs
	InitrdPath string
	PmemPaths  []string

	// Snapshot
	SnapfilePath string

	// Vsock
	VsockCID        uint32
	VsockSocketPath string

	// Sandbox ID (passed via kernel cmdline)
	SandboxId string

	EventMonitorFd int
	ApiSocketFd    int
}

type Adapter interface {
	BuildStartCmd(args *ResourceArgs, isResume bool) (string, error)
	PrepareLaunch(args *ResourceArgs) error
	AfterProcessStart()
	WaitForCreateReady(ctx context.Context, processExited <-chan error) error
	WaitForResumeReady(ctx context.Context, processExited <-chan error) error
	CheckAgentAlive(ctx context.Context, processExited <-chan error) error
	PauseVM() error
	ResumeVM() error
	DeleteVM() error
	CreateSnapshot(snapfilePath string) error
	LoadSnapshot(snapfilePath string, preferVNC bool) error
	Cleanup()
}
