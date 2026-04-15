package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/openeuler/Conch/internal/sandbox/network"
	"github.com/openeuler/Conch/internal/sandbox/vmm"
	"github.com/openeuler/Conch/internal/snapshot"
)

const (
	defaultCPUBoot = 1
	// CID 0 = hypervisor, 1 = reserved, 2 = host
	vsockCIDOffset = 3
	// VsockSocketDir is the directory for vsock socket files
	VsockSocketDir = "/var/run/conch"
)

// SandboxVsockSocketPath returns the vsock socket path for a sandbox.
func SandboxVsockSocketPath(sandboxId string) (string, error) {
	if err := os.MkdirAll(VsockSocketDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create vsock socket directory: %w", err)
	}

	return filepath.Join(VsockSocketDir, fmt.Sprintf("conch-vmm-%s.vsock", sandboxId)), nil
}

type Execution struct {
	Logs string `json:"logs"`
}

type Sandbox struct {
	cleanup      *Cleanup
	process      *vmm.Process
	snapshotConf *snapshot.SnapshotConfig
	namespace    string
	slot         *network.Slot
	vsockConn    net.Conn
}

func ResumeSandbox(
	ctx context.Context,
	snapshotConf *snapshot.SnapshotConfig,
	namespace, vmmName, sandboxId string, vcpuNum int64, pool *network.Pool,
	vsockCID uint32, vsockSocketPath string,
) (s *Sandbox, e error) {
	cleanup := NewCleanup()
	defer func() {
		if e != nil {
			cleanupErr := cleanup.Run(ctx)
			e = errors.Join(e, cleanupErr)
		}
	}()

	slot, err := pool.Get(ctx)
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

	snapfilePath := snapshotConf.SnapDir()

	vmmResourceArgs := &vmm.ResourceArgs{
		CPUBoot:         defaultCPUBoot,
		CPUMax:          vcpuNum,
		MemorySize:      snapshotConf.MemSize,
		MemoryPath:      snapshotConf.SnapshotMemFile(),
		NamespaceID:     slot.NamespaceID(),
		TapName:         slot.TapName(),
		KernelPath:      snapshotConf.KernelFile(),
		SnapfilePath:    snapfilePath,
		InitrdPath:      snapshotConf.InitrdFile(),
		PmemPaths:       snapshotConf.PmemFiles(),
		VsockCID:        vsockCID,
		VsockSocketPath: vsockSocketPath,
	}

	vmmHandle, vmmErr := vmm.NewProcess(
		vmmName, sandboxId, vmmResourceArgs, true,
	)
	if vmmErr != nil {
		return nil, fmt.Errorf("failed to init VMM: %w", vmmErr)
	}

	err = vmmHandle.Resume(ctx, snapfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create VMM: %w", err)
	}

	sbx := &Sandbox{
		snapshotConf: snapshotConf,
		process:      vmmHandle,
		cleanup:      cleanup,
		namespace:    namespace,
		slot:         slot,
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
	snapshotConf *snapshot.SnapshotConfig,
	namespace, vmmName, sandboxId string, vcpuNum int64, pool *network.Pool,
	vsockCID uint32, vsockSocketPath string,
) (s *Sandbox, e error) {

	cleanup := NewCleanup()
	defer func() {
		if e != nil {
			cleanupErr := cleanup.Run(ctx)
			e = errors.Join(e, cleanupErr)
		}
	}()

	slot, err := pool.Get(ctx)
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
		CPUBoot:         defaultCPUBoot,
		CPUMax:          vcpuNum,
		MemorySize:      snapshotConf.MemSize,
		MemoryPath:      snapshotConf.SnapshotMemFile(),
		NamespaceID:     slot.NamespaceID(),
		TapName:         slot.TapName(),
		KernelPath:      snapshotConf.KernelFile(),
		InitrdPath:      snapshotConf.InitrdFile(),
		PmemPaths:       snapshotConf.PmemFiles(),
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
		snapshotConf: snapshotConf,
		process:      vmmHandle,
		cleanup:      cleanup,
		namespace:    namespace,
		slot:         slot,
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
	if err := s.process.Pause(ctx); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}

	err := s.process.CreateSnapshot(ctx, s.snapshotConf.SnapDir())
	if err != nil {
		return fmt.Errorf("error creating snapshot: %w", err)
	}

	return nil
}
