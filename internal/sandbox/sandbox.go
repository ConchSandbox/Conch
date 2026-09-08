package sandbox

import (
	"context"
	"fmt"

	"github.com/openeuler/Conch/internal/agent/hostconn"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/vmm"
	"github.com/openeuler/Conch/internal/vmm/driver"
)

const (
	minVCPUNum = 1
	// CID 0 = hypervisor, 1 = reserved, 2 = host
	vsockCIDOffset = 3
)

func SandboxVsockSocketPath(sandboxId string) (string, error) {
	return vmm.SandboxSocketPath("x", sandboxId)
}

func validateVCPUNum(vcpuNum, vcpuMax int64) error {
	if vcpuNum < minVCPUNum {
		return fmt.Errorf("vcpu_num must be at least %d, got %d", minVCPUNum, vcpuNum)
	}
	if vcpuMax < vcpuNum {
		return fmt.Errorf("vcpu_max must be at least vcpu_num (%d), got %d", vcpuNum, vcpuMax)
	}
	return nil
}

type Execution struct {
	Logs string `json:"logs"`
}

type VMStartSpec struct {
	MemorySizeMB int64

	MemoryPath   string
	KernelPath   string
	InitrdPath   string
	SnapfilePath string
	PmemPaths    []string
	VirtioFS     []driver.VirtioFSDevice
}

type Sandbox struct {
	cleanup     *Cleanup
	process     *vmm.Process
	vmStartSpec VMStartSpec
	vmmName     string
	sandboxID   string
	slot        *netstack.Slot
}

// launchSandbox returns acquired resources even on failure. Manager owns
// rollback, including a VMM that was started but failed to become ready.
func launchSandbox(ctx context.Context, req CreateRequest, spec VMStartSpec, vmmBinary string,
	pool *netstack.Pool, runtimeIDs createRuntimeIDs, readyOpts *hostconn.ReadyOptions, restore bool,
) (*Sandbox, error) {
	sbx := &Sandbox{cleanup: NewCleanup(), vmStartSpec: spec, vmmName: req.VMMName, sandboxID: req.SandboxID}
	slot, err := pool.Get(ctx, req.SandboxID, req.Network)
	if err != nil {
		return sbx, fmt.Errorf("prepare network: %w", err)
	}
	sbx.slot = slot
	sbx.cleanup.Add(func(ctx context.Context) error { return pool.Release(ctx, slot) })
	readyOpts.Network = slot.GuestNetworkConfig()
	if _, err := hostconn.ValidateReadyRequest(*readyOpts); err != nil {
		return sbx, fmt.Errorf("validate initialization before VMM start: %w", err)
	}
	resources := &vmm.ResourceArgs{
		CPUBoot: req.VCPUNum, CPUMax: req.VCPUMax, MemorySize: spec.MemorySizeMB, MemoryPath: spec.MemoryPath,
		NetNSPath: slot.NetNSPath(), TapName: slot.TapName(), KernelPath: spec.KernelPath, InitrdPath: spec.InitrdPath,
		PmemPaths: append([]string(nil), spec.PmemPaths...), VirtioFS: append([]driver.VirtioFSDevice(nil), spec.VirtioFS...),
		VsockCID: runtimeIDs.vsockCID, VsockSocketPath: runtimeIDs.vsockSocketPath, SandboxId: req.SandboxID,
	}
	if restore {
		resources.SnapfilePath = spec.SnapfilePath
	}
	process, err := vmm.NewProcess(req.VMMName, vmmBinary, req.SandboxID, resources, restore)
	if err != nil {
		return sbx, fmt.Errorf("prepare VMM: %w", err)
	}
	sbx.process = process
	sbx.cleanup.Add(func(context.Context) error { return cleanupFiles(process.VmmSocketPath, process.VsockSocketPath) })
	sbx.cleanup.AddPriority(func(ctx context.Context) error {
		if process.Pid() == 0 {
			return nil
		}
		return sbx.Stop(ctx)
	})
	if restore {
		err = process.Restore(ctx, spec.SnapfilePath)
	} else {
		err = process.Create(ctx)
	}
	if err != nil {
		return sbx, fmt.Errorf("start VMM: %w", err)
	}
	return sbx, nil
}

func (s *Sandbox) Wait(ctx context.Context) error {
	return s.process.Wait()
}

func (s *Sandbox) Stop(ctx context.Context) error {
	vmmStopErr := s.process.Stop()
	if vmmStopErr != nil {
		return fmt.Errorf("failed to stop VMM: %w", vmmStopErr)
	}

	return nil
}

func (s *Sandbox) Close(ctx context.Context) error {
	err := s.cleanup.Run(ctx)
	if err != nil {
		return fmt.Errorf("failed to cleanup sandbox: %w", err)
	}
	return nil
}

func (s *Sandbox) Pause(ctx context.Context) error {
	return s.Suspend(ctx)
}

func (s *Sandbox) Suspend(ctx context.Context) error {
	if err := s.process.Pause(ctx); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}
	return nil
}

func (s *Sandbox) Resume(ctx context.Context) error {
	if err := s.process.ResumeVM(ctx); err != nil {
		return fmt.Errorf("failed to resume VM: %w", err)
	}
	return nil
}

// CreateVMMState writes the VMM-specific capture into snapshotDir. The caller
// is responsible for pausing and resuming the sandbox around this operation.
func (s *Sandbox) CreateVMMState(ctx context.Context, snapshotDir string) error {
	if s == nil || s.process == nil {
		return fmt.Errorf("sandbox VMM process is not configured")
	}
	if err := s.process.CreateSnapshot(ctx, snapshotDir); err != nil {
		return fmt.Errorf("create VMM state: %w", err)
	}
	return nil
}

// MemoryBackingPath returns the external memory backing used by the VMM.
func (s *Sandbox) MemoryBackingPath() string {
	if s == nil {
		return ""
	}
	return s.vmStartSpec.MemoryPath
}

// MemorySizeMB returns the immutable Guest RAM size used for this runtime.
func (s *Sandbox) MemorySizeMB() int64 {
	if s == nil {
		return 0
	}
	return s.vmStartSpec.MemorySizeMB
}

// VMMName returns the driver name needed to interpret the captured VMM state.
func (s *Sandbox) VMMName() string {
	if s == nil {
		return ""
	}
	return s.vmmName
}
