package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/internal/vmm"
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
}

func vmStartSpecFromBootSpec(spec template.SandboxBootSpec) VMStartSpec {
	return VMStartSpec{
		MemorySizeMB: spec.MemorySizeMB,
		MemoryPath:   spec.MemoryPath,
		KernelPath:   spec.KernelPath,
		InitrdPath:   spec.InitrdPath,
		SnapfilePath: spec.SnapfilePath,
		PmemPaths:    append([]string(nil), spec.PmemPaths...),
	}
}

func vmStartSpecFromRecord(rec state.SandboxRecord) VMStartSpec {
	spec := template.BootSpecFromRuntime(template.SandboxBootRuntime{
		RootfsMount: rec.RootfsMount,
		MemMount:    rec.MemMount,
		VMMount:     rec.VMMount,
		RootDir:     rec.SnapshotRootDir,
		MemSize:     rec.RamMB,
	})
	return vmStartSpecFromBootSpec(spec)
}

type Sandbox struct {
	cleanup     *Cleanup
	process     *vmm.Process
	vmStartSpec VMStartSpec
	namespace   string
	sandboxID   string
	leaseID     string
	slot        *netstack.Slot
	vsockConn   net.Conn
}

func attachSandboxFromRecord(rec state.SandboxRecord, pool *netstack.Pool) (*Sandbox, error) {
	if rec.VMMName == "" {
		return nil, fmt.Errorf("missing vmm name")
	}
	if rec.VMMSocketPath == "" {
		return nil, fmt.Errorf("missing vmm socket path")
	}
	if rec.NetworkSlotKey == "" {
		return nil, fmt.Errorf("missing network slot key")
	}
	slotIdx, err := strconv.Atoi(rec.NetworkSlotKey)
	if err != nil {
		return nil, fmt.Errorf("invalid network slot key %q: %w", rec.NetworkSlotKey, err)
	}
	slot, err := netstack.NewSlot(rec.NetworkSlotKey, slotIdx)
	if err != nil {
		return nil, err
	}
	process, err := vmm.NewAttachedProcess(rec.VMMName, rec.VMMSocketPath, rec.VsockSocketPath, rec.VMMPID)
	if err != nil {
		return nil, err
	}
	vmStartSpec := vmStartSpecFromRecord(rec)
	cleanup := NewCleanup()
	sb := &Sandbox{
		cleanup:     cleanup,
		process:     process,
		vmStartSpec: vmStartSpec,
		namespace:   rec.Namespace,
		sandboxID:   rec.ConchSandboxID,
		leaseID:     rec.LeaseID,
		slot:        slot,
	}
	cleanup.Add(func(ctx context.Context) error {
		if pool == nil || slot == nil {
			return nil
		}
		return pool.Release(ctx, slot)
	})
	cleanup.Add(func(ctx context.Context) error {
		if err := cleanupFiles(process.VmmSocketPath, process.VsockSocketPath); err != nil {
			return fmt.Errorf("failed to cleanup files: %w", err)
		}
		return nil
	})
	cleanup.AddPriority(func(ctx context.Context) error {
		return sb.Stop(ctx)
	})
	return sb, nil
}

func ResumeSandbox(
	ctx context.Context,
	vmStartSpec VMStartSpec,
	namespace, vmmName, sandboxId string, vcpuNum, vcpuMax int64, pool *netstack.Pool,
	vsockCID uint32, vsockSocketPath string,
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

	slot, err := pool.Get(ctx, sandboxId)
	if err != nil {
		return nil, fmt.Errorf("failed to init network: %w", err)
	}

	cleanup.Add(func(ctx context.Context) error {
		err := pool.Release(ctx, slot)
		if err != nil {
			return fmt.Errorf("failed to release network slot %s: %w", slot.Key, err)
		}
		return nil
	})

	vmmResourceArgs := &vmm.ResourceArgs{
		CPUBoot:         vcpuNum,
		CPUMax:          vcpuMax,
		MemorySize:      vmStartSpec.MemorySizeMB,
		MemoryPath:      vmStartSpec.MemoryPath,
		NamespaceID:     slot.NamespaceID(),
		TapName:         slot.TapName(),
		KernelPath:      vmStartSpec.KernelPath,
		SnapfilePath:    vmStartSpec.SnapfilePath,
		InitrdPath:      vmStartSpec.InitrdPath,
		PmemPaths:       append([]string(nil), vmStartSpec.PmemPaths...),
		VsockCID:        vsockCID,
		VsockSocketPath: vsockSocketPath,
	}

	vmmHandle, vmmErr := vmm.NewProcess(
		vmmName, sandboxId, vmmResourceArgs, true,
	)
	if vmmErr != nil {
		return nil, fmt.Errorf("failed to init VMM: %w", vmmErr)
	}

	err = vmmHandle.Resume(ctx, vmStartSpec.SnapfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create VMM: %w", err)
	}

	sbx := &Sandbox{
		vmStartSpec: vmStartSpec,
		process:     vmmHandle,
		cleanup:     cleanup,
		namespace:   namespace,
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
	namespace, vmmName, sandboxId string, vcpuNum, vcpuMax int64, pool *netstack.Pool,
	vsockCID uint32, vsockSocketPath string,
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

	slot, err := pool.Get(ctx, sandboxId)
	if err != nil {
		return nil, fmt.Errorf("failed to init network: %w", err)
	}

	cleanup.Add(func(ctx context.Context) error {
		err := pool.Release(ctx, slot)
		if err != nil {
			return fmt.Errorf("failed to release network slot %s: %w", slot.Key, err)
		}
		return nil
	})

	vmmResourceArgs := &vmm.ResourceArgs{
		CPUBoot:         vcpuNum,
		CPUMax:          vcpuMax,
		MemorySize:      vmStartSpec.MemorySizeMB,
		MemoryPath:      vmStartSpec.MemoryPath,
		NamespaceID:     slot.NamespaceID(),
		TapName:         slot.TapName(),
		KernelPath:      vmStartSpec.KernelPath,
		InitrdPath:      vmStartSpec.InitrdPath,
		PmemPaths:       append([]string(nil), vmStartSpec.PmemPaths...),
		VsockCID:        vsockCID,
		VsockSocketPath: vsockSocketPath,
		SandboxId:       sandboxId,
	}

	vmmHandle, vmmErr := vmm.NewProcess(
		vmmName, sandboxId, vmmResourceArgs, false,
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
		namespace:   namespace,
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
	if s.vsockConn != nil {
		s.vsockConn.Close()
		s.vsockConn = nil
	}
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

// captureCheckpoint writes a self-contained VMM snapshot to a temporary
// directory. If it pauses a running VM, the VM remains paused on success so
// the caller can atomically commit the capture before resuming it.
func (s *Sandbox) captureCheckpoint(ctx context.Context, pauseBefore bool) (string, error) {
	stagingDir, err := os.MkdirTemp("", "conch-vm-snapshot-*")
	if err != nil {
		return "", fmt.Errorf("create snapshot staging directory: %w", err)
	}

	if pauseBefore {
		if err := s.process.Pause(ctx); err != nil {
			_ = os.RemoveAll(stagingDir)
			return "", fmt.Errorf("failed to pause VM: %w", err)
		}
	}

	if err := s.process.CreateSnapshot(ctx, stagingDir); err != nil {
		_ = os.RemoveAll(stagingDir)
		if pauseBefore {
			_ = s.process.ResumeVM(context.WithoutCancel(ctx))
		}
		return "", fmt.Errorf("error creating snapshot: %w", err)
	}

	return stagingDir, nil
}
