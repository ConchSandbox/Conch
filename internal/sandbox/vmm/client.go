package vmm

import (
	"fmt"
)

const (
	CLHVmmType = 0
	StratovirtVmmType = 1
)

var vmmTypeMap = map[string]int{
	"cloud-hypervisor": CLHVmmType,
	"stratovirt": StratovirtVmmType,
}

type ResourceArgs struct {
	// CPU
	CPUBoot		int64
	CPUMax		int64

	// Memory
	MemorySize int64
	MemoryPath string

	// Net
	NamespaceID string
	TapName     string

}

func GetVmmType(vmmName string) (int, bool){
	// debug
	fmt.Printf("try vmm type %s \n", vmmName)

	vmmType, exists := vmmTypeMap[vmmName]
	return vmmType, exists
}

type vmmClient interface {
	BuildStartCmd(args *ResourceArgs, RootfsSock, KernelPath string, DiskPath string, isResume bool) (string, error)
    CheckDaemonAlive() error
	PauseVM() error
	ResumeVM() error
	DeleteVM() error
	CreateSnapshot(snapfilePath string) error
	LoadSnapshot(snapfilePath string, prefault bool) error
}

func newVmmClient(vmmType int, vmmSocketPath string) (vmmClient, error) {
	switch vmmType {
    case CLHVmmType:
		// debug
		fmt.Println("New CLH Client...")

        return NewCLHClient(vmmType, vmmSocketPath), nil
    case StratovirtVmmType:
        return nil, fmt.Errorf("not support vmm type: %d", vmmType)
    default:
        return nil, fmt.Errorf("unknown vmm type: %d", vmmType)
    }
}