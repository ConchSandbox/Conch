package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/openeuler/Conch/internal/agent/hostconn"
	"github.com/openeuler/Conch/internal/memsnap"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/vmm"
	"github.com/openeuler/Conch/internal/vmm/driver"
)

const (
	minVCPUNum = 1
	// CID 0 = hypervisor, 1 = reserved, 2 = host
	vsockCIDOffset = 3
)

func SandboxVsockSocketPath(workDir, sandboxId string) (string, error) {
	return vmm.SandboxSocketPath(workDir, "x", sandboxId)
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
	MemorySizeMB      int64
	MemoryFormat      string
	MemoryFile        *os.File
	UFFDSocketPath    string
	ResumeStartedHook func(context.Context) error

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
		MemoryFormat: spec.MemoryFormat,
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

	memoryMu           sync.RWMutex
	memoryMode         string
	memoryManifest     *memsnap.Manifest
	memorySizeBytes    uint64
	memoryBlockSize    uint64
	checkpointPoisoned atomic.Bool
}

func RestoreSandbox(
	ctx context.Context,
	workDir string,
	vmStartSpec VMStartSpec,
	vmmName, vmmBinary, sandboxId string, vcpuNum, vcpuMax int64, pool *netstack.Pool,
	vsockCID uint32, vsockSocketPath string, network *netstack.SandboxNetworkConfig,
	readyOpts *hostconn.ReadyOptions,
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
	readyOpts.Network = slot.GuestNetworkConfig()
	if _, err := hostconn.ValidateReadyRequest(*readyOpts); err != nil {
		return nil, fmt.Errorf("validate initialization before VMM start: %w", err)
	}

	var incrementalRestore *vmm.IncrementalRestoreArgs
	if vmStartSpec.MemoryFile != nil || vmStartSpec.UFFDSocketPath != "" || vmStartSpec.ResumeStartedHook != nil {
		incrementalRestore = &vmm.IncrementalRestoreArgs{
			MemoryFile:        vmStartSpec.MemoryFile,
			UFFDSocketPath:    vmStartSpec.UFFDSocketPath,
			ResumeStartedHook: vmStartSpec.ResumeStartedHook,
		}
	}
	vmmResourceArgs := &vmm.ResourceArgs{
		CPUBoot:            vcpuNum,
		CPUMax:             vcpuMax,
		MemorySize:         vmStartSpec.MemorySizeMB,
		MemoryPath:         vmStartSpec.MemoryPath,
		MemoryFormat:       vmStartSpec.MemoryFormat,
		IncrementalRestore: incrementalRestore,
		NetNSPath:          slot.NetNSPath(),
		TapName:            slot.TapName(),
		KernelPath:         vmStartSpec.KernelPath,
		SnapfilePath:       vmStartSpec.SnapfilePath,
		InitrdPath:         vmStartSpec.InitrdPath,
		PmemPaths:          append([]string(nil), vmStartSpec.PmemPaths...),
		VirtioFS:           append([]driver.VirtioFSDevice(nil), vmStartSpec.VirtioFS...),
		VsockCID:           vsockCID,
		VsockSocketPath:    vsockSocketPath,
	}

	vmmHandle, vmmErr := vmm.NewProcess(
		workDir, vmmName, vmmBinary, sandboxId, vmmResourceArgs, true,
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
	workDir string,
	vmStartSpec VMStartSpec,
	vmmName, vmmBinary, sandboxId string, vcpuNum, vcpuMax int64, pool *netstack.Pool,
	vsockCID uint32, vsockSocketPath string, network *netstack.SandboxNetworkConfig,
	readyOpts *hostconn.ReadyOptions,
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
	readyOpts.Network = slot.GuestNetworkConfig()
	if _, err := hostconn.ValidateReadyRequest(*readyOpts); err != nil {
		return nil, fmt.Errorf("validate initialization before VMM start: %w", err)
	}

	vmmResourceArgs := &vmm.ResourceArgs{
		CPUBoot:         vcpuNum,
		CPUMax:          vcpuMax,
		MemorySize:      vmStartSpec.MemorySizeMB,
		MemoryPath:      vmStartSpec.MemoryPath,
		MemoryFormat:    vmStartSpec.MemoryFormat,
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
		workDir, vmmName, vmmBinary, sandboxId, vmmResourceArgs, false,
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

func (s *Sandbox) SetCheckpointPoisoned(poisoned bool) {
	if s != nil {
		s.checkpointPoisoned.Store(poisoned)
	}
}

func (s *Sandbox) CheckpointPoisoned() bool {
	return s != nil && s.checkpointPoisoned.Load()
}

func (s *Sandbox) SetIncrementalManifest(manifest memsnap.Manifest) {
	if s == nil {
		return
	}
	s.memoryMu.Lock()
	s.memoryManifest = &manifest
	s.memoryMu.Unlock()
}

func (s *Sandbox) IncrementalMemoryRuntime() (IncrementalMemoryRuntime, error) {
	if s == nil || s.process == nil {
		return IncrementalMemoryRuntime{}, fmt.Errorf("sandbox VMM process is not configured")
	}
	adapter, ok := s.process.IncrementalMemoryAdapter()
	if !ok {
		return IncrementalMemoryRuntime{}, fmt.Errorf("VMM does not support incremental memory capture")
	}
	s.memoryMu.RLock()
	defer s.memoryMu.RUnlock()
	var parent *memsnap.Manifest
	if s.memoryManifest != nil {
		copy := *s.memoryManifest
		copy.Layers = append([]string(nil), copy.Layers...)
		copy.BuildMap = append([]memsnap.BuildRange(nil), copy.BuildMap...)
		parent = &copy
	}
	return IncrementalMemoryRuntime{
		ParentManifest: parent,
		MemorySize:     s.memorySizeBytes, BlockSize: s.memoryBlockSize,
		PID: s.process.Pid(), Adapter: adapter,
	}, nil
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
