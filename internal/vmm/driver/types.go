package driver

import (
	"context"
	"os"
)

type ResourceArgs struct {
	// CPU
	CPUBoot int64
	CPUMax  int64

	// Memory
	MemorySize        int64
	MemoryPath        string
	MemoryFile        *os.File
	MemoryFD          int
	MemoryFormat      string
	UFFDSocketPath    string
	ResumeStartedHook func(context.Context) error

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

type MemoryMapping struct {
	BaseHostVirtualAddress uint64 `json:"base-host-virt-addr"`
	Size                   uint64 `json:"size"`
	Offset                 uint64 `json:"offset"`
	PageSize               uint64 `json:"page-size"`
}

type MemoryPageState struct {
	Resident []uint64 `json:"resident,omitempty"`
	Empty    []uint64 `json:"empty,omitempty"`
	PageSize uint64   `json:"page-size"`
}

type MemoryDirtyBitmap struct {
	Bitmap   []uint64 `json:"bitmap,omitempty"`
	PageSize uint64   `json:"page-size"`
}

type IncrementalMemoryAdapter interface {
	CreateExternalMemorySnapshot(snapfilePath string) error
	QueryMemoryMappings() ([]MemoryMapping, error)
	QueryMemoryPageState() (MemoryPageState, error)
	QueryMemoryDirtyBitmap() (MemoryDirtyBitmap, error)
}

type Adapter interface {
	BuildStartCmd(args *ResourceArgs, restore bool) (string, error)
	PrepareLaunch(args *ResourceArgs, restore bool) error
	AfterProcessStart()
	WaitForCreateReady(ctx context.Context, processExited <-chan error) error
	WaitForRestoreReady(ctx context.Context, processExited <-chan error) error
	CheckAgentAlive(ctx context.Context, processExited <-chan error) error
	PauseVM() error
	ResumeVM() error
	DeleteVM() error
	CreateSnapshot(snapfilePath string) error
	LoadSnapshot(snapfilePath string, preferVNC bool) error
	Cleanup()
}
