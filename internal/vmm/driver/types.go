package driver

import "context"

type ProcessExit interface {
	Done() <-chan struct{}
	Err() error
}

type ResourceArgs struct {
	// CPU
	CPUBoot int64
	CPUMax  int64

	// Memory
	MemorySize int64
	MemoryPath string

	// Net
	NetNSPath string
	TapName   string

	// Kernel
	KernelPath string

	// Rootfs
	InitrdPath string
	PmemPaths  []string
	VirtioFS   []VirtioFSDevice

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

type VirtioFSDevice struct {
	Tag    string
	Socket string
}

type Adapter interface {
	BuildStartCmd(args *ResourceArgs, restore bool) (string, error)
	PrepareLaunch(args *ResourceArgs, restore bool) error
	AfterProcessStart()
	WaitForCreateReady(ctx context.Context, processExited ProcessExit) error
	WaitForRestoreReady(ctx context.Context, processExited ProcessExit) error
	CheckAgentAlive(ctx context.Context, processExited ProcessExit) error
	PauseVM() error
	ResumeVM() error
	DeleteVM() error
	CreateSnapshot(snapfilePath string) error
	LoadSnapshot(snapfilePath string, preferVNC bool) error
	Cleanup()
}
