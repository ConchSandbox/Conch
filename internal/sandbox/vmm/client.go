package vmm

import (
	"fmt"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	CLHVmmType        = 0
	StratovirtVmmType = 1
)

var vmmTypeMap = map[string]int{
	"cloud-hypervisor": CLHVmmType,
	"stratovirt":       StratovirtVmmType,
}

func GetVmmType(vmmName string) (int, bool) {
	logger := ulog.GetLogger()
	logger.Debug("Getting VMM type", ulog.F("vmm_name", vmmName))
	vmmType, exists := vmmTypeMap[vmmName]
	return vmmType, exists
}

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
}

type vmmClient interface {
	BuildStartCmd(args *ResourceArgs, isResume bool) (string, error)
	CheckDaemonAlive() error
	PauseVM() error
	ResumeVM() error
	DeleteVM() error
	CreateSnapshot(snapfilePath string) error
	LoadSnapshot(snapfilePath string, preferVNC bool) error
}

func newVmmClient(vmmType int, vmmSocketPath string) (vmmClient, error) {
	switch vmmType {
	case CLHVmmType:
		logger := ulog.GetLogger()
		logger.Info("Creating CLH client", ulog.F("socket", vmmSocketPath))
		return NewCLHClient(vmmType, vmmSocketPath), nil
	case StratovirtVmmType:
		ulog.GetLogger().Error("Unsupported VMM type",
			ulog.F("type", vmmType),
		)
		return nil, fmt.Errorf("unsupported VMM type: %s", vmmType)
	default:
		ulog.GetLogger().Error("Unknown VMM type",
			ulog.F("type", vmmType),
		)
		return nil, fmt.Errorf("unknown VMM type: %s", vmmType)
	}
}
