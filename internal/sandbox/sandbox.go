package sandbox

import (
	"context"
	"errors"
	"fmt"

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

func vmStartSpecFromBootSpec(spec BootSpec) VMStartSpec {
	return VMStartSpec{
		MemorySizeMB: spec.MemorySizeMB,
		MemoryPath:   spec.MemoryPath,
		KernelPath:   spec.KernelPath,
		InitrdPath:   spec.InitrdPath,
		SnapfilePath: spec.SnapfilePath,
		PmemPaths:    append([]string(nil), spec.PmemPaths...),
	}
}

type Sandbox struct {
	cleanup     *Cleanup
	process     *vmm.Process
	vmStartSpec VMStartSpec
	vmmName     string
	sandboxID   string
	leaseID     string
	slot        *netstack.Slot
}

func RestoreSandbox(
	ctx context.Context,
	vmStartSpec VMStartSpec,
	vmmName, vmmBinary, sandboxId string, vcpuNum, vcpuMax int64, pool *netstack.Pool,
	vsockCID uint32, vsockSocketPath string, network *netstack.SandboxNetworkConfig,
) (s *Sandbox, e error) {
	if err := validateVCPUNum(vcpuNum, vcpuMax); err != nil {
		return nil, fmt.Errorf("invalid vcpu configuration: %w", err)
	}

	cleanup := NewCleanup()
	defer func() {
		if e != nil {
			cleanupErr := cleanup.Run(context.WithoutCancel(ctx))
			e = errors.Join(e, cleanupErr)
		}
	}()

	slot, err := pool.Get(ctx, sandboxId, network)
	if err != nil {
		return nil, fmt.Errorf("failed to init network: %w", err)
	}

	cleanup.Add(func(ctx context.Context) error {
		err := pool.Release(ctx, slot)
		if err != nil {
			return fmt.Errorf("failed to release network slot %d: %w", slot.ID(), err)
		}
		return nil
	})

	vmmResourceArgs := &vmm.ResourceArgs{
		CPUBoot:         vcpuNum,
		CPUMax:          vcpuMax,
		MemorySize:      vmStartSpec.MemorySizeMB,
		MemoryPath:      vmStartSpec.MemoryPath,
		NetNSPath:       slot.NetNSPath(),
		TapName:         slot.TapName(),
		KernelPath:      vmStartSpec.KernelPath,
		SnapfilePath:    vmStartSpec.SnapfilePath,
		InitrdPath:      vmStartSpec.InitrdPath,
		PmemPaths:       append([]string(nil), vmStartSpec.PmemPaths...),
		VirtioFS:        append([]driver.VirtioFSDevice(nil), vmStartSpec.VirtioFS...),
		VsockCID:        vsockCID,
		VsockSocketPath: vsockSocketPath,
	}

	vmmHandle, vmmErr := vmm.NewProcess(
		vmmName, vmmBinary, sandboxId, vmmResourceArgs, true,
	)
	if vmmErr != nil {
		return nil, fmt.Errorf("failed to init VMM: %w", vmmErr)
	}

	err = vmmHandle.Restore(ctx, vmStartSpec.SnapfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to restore VMM: %w", err)
	}

	sbx := &Sandbox{
		vmStartSpec: vmStartSpec,
		process:     vmmHandle,
		cleanup:     cleanup,
		vmmName:     vmmName,
		sandboxID:   sandboxId,
		slot:        slot,
	}

	cleanup.Add(func(ctx context.Context) error {
		filesErr := cleanupFiles(sbx.process.VmmSocketPath, sbx.process.VsockSocketPath)
		if filesErr != nil {
			return fmt.Errorf("failed to cleanup files: %w", filesErr)
		}

		return nil
	})
	cleanup.AddPriority(func(ctx context.Context) error {
		// Stop the sandbox first if it is still running, otherwise do nothing
		return sbx.Stop(ctx)
	})

	return sbx, nil
}

func CreateSandbox(
	ctx context.Context,
	vmStartSpec VMStartSpec,
	vmmName, vmmBinary, sandboxId string, vcpuNum, vcpuMax int64, pool *netstack.Pool,
	vsockCID uint32, vsockSocketPath string, network *netstack.SandboxNetworkConfig,
) (s *Sandbox, e error) {

	if err := validateVCPUNum(vcpuNum, vcpuMax); err != nil {
		return nil, fmt.Errorf("invalid vcpu configuration: %w", err)
	}

	cleanup := NewCleanup()
	defer func() {
		if e != nil {
			cleanupErr := cleanup.Run(context.WithoutCancel(ctx))
			e = errors.Join(e, cleanupErr)
		}
	}()

	slot, err := pool.Get(ctx, sandboxId, network)
	if err != nil {
		return nil, fmt.Errorf("failed to init network: %w", err)
	}

	cleanup.Add(func(ctx context.Context) error {
		err := pool.Release(ctx, slot)
		if err != nil {
			return fmt.Errorf("failed to release network slot %d: %w", slot.ID(), err)
		}
		return nil
	})

	vmmResourceArgs := &vmm.ResourceArgs{
		CPUBoot:         vcpuNum,
		CPUMax:          vcpuMax,
		MemorySize:      vmStartSpec.MemorySizeMB,
		MemoryPath:      vmStartSpec.MemoryPath,
		NetNSPath:       slot.NetNSPath(),
		TapName:         slot.TapName(),
		KernelPath:      vmStartSpec.KernelPath,
		InitrdPath:      vmStartSpec.InitrdPath,
		PmemPaths:       append([]string(nil), vmStartSpec.PmemPaths...),
		VirtioFS:        append([]driver.VirtioFSDevice(nil), vmStartSpec.VirtioFS...),
		VsockCID:        vsockCID,
		VsockSocketPath: vsockSocketPath,
		SandboxId:       sandboxId,
	}

	vmmHandle, vmmErr := vmm.NewProcess(
		vmmName, vmmBinary, sandboxId, vmmResourceArgs, false,
	)
	if vmmErr != nil {
		return nil, fmt.Errorf("failed to init VMM: %w", vmmErr)
	}

	err = vmmHandle.Create(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create VMM: %w", err)
	}

	sbx := &Sandbox{
		vmStartSpec: vmStartSpec,
		process:     vmmHandle,
		cleanup:     cleanup,
		vmmName:     vmmName,
		sandboxID:   sandboxId,
		slot:        slot,
	}

	cleanup.Add(func(ctx context.Context) error {
		filesErr := cleanupFiles(sbx.process.VmmSocketPath, sbx.process.VsockSocketPath)
		if filesErr != nil {
			return fmt.Errorf("failed to cleanup files: %w", filesErr)
		}

		return nil
	})
	cleanup.AddPriority(func(ctx context.Context) error {
		// Stop the sandbox first if it is still running, otherwise do nothing
		return sbx.Stop(ctx)
	})

	return sbx, nil
}

func (s *Sandbox) Wait(ctx context.Context) error {
	s.process.Wait()
	return nil
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
