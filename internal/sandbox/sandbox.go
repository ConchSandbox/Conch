package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/vmm"
)

const (
	minVCPUNum = 1
	// CID 0 = hypervisor, 1 = reserved, 2 = host
	vsockCIDOffset = 3
)

func SandboxVsockSocketPath(sandboxId string) (string, error) {
	socketDir, err := vmm.EnsureWorkSubDir("vsock")
	if err != nil {
		return "", err
	}
	return filepath.Join(socketDir, fmt.Sprintf("conch-vmm-%s.vsock", sandboxId)), nil
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

type Sandbox struct {
	cleanup      *Cleanup
	process      *vmm.Process
	snapshotConf *snapshot.SnapshotConfig
	namespace    string
	sandboxID    string
	leaseID      string
	slot         *netstack.Slot
	vsockConn    net.Conn
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
	rootDir := rec.SnapshotRootDir
	if rootDir == "" {
		rootDir = "/conch/snapshot"
	}
	memSize := rec.RamMB
	if memSize <= 0 {
		memSize = 256
	}
	conf := &snapshot.SnapshotConfig{
		Rootfs:  rec.RootfsMount,
		MemDir:  rec.MemMount,
		VmDir:   rec.VMMount,
		RootDir: rootDir,
		MemSize: memSize,
	}
	cleanup := NewCleanup()
	sb := &Sandbox{
		cleanup:      cleanup,
		process:      process,
		snapshotConf: conf,
		namespace:    rec.Namespace,
		sandboxID:    rec.ConchSandboxID,
		leaseID:      rec.LeaseID,
		slot:         slot,
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
	snapshotConf *snapshot.SnapshotConfig,
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

	snapfilePath := snapshotConf.SnapDir()

	vmmResourceArgs := &vmm.ResourceArgs{
		CPUBoot:         vcpuNum,
		CPUMax:          vcpuMax,
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
		sandboxID:    sandboxId,
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
		sandboxID:    sandboxId,
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
	stagingDir, err := os.MkdirTemp("", "conch-vm-snapshot-*")
	if err != nil {
		return fmt.Errorf("create snapshot staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	if err := s.process.Pause(ctx); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}

	if err := s.process.CreateSnapshot(ctx, stagingDir); err != nil {
		return fmt.Errorf("error creating snapshot: %w", err)
	}
	if err := s.Stop(ctx); err != nil {
		return fmt.Errorf("failed to stop VM after snapshot: %w", err)
	}
	if err := replaceDirWithRetry(stagingDir, s.snapshotConf.SnapDir(), time.Second, 20*time.Millisecond); err != nil {
		return fmt.Errorf("stage snapshot into mem snapshot: %w", err)
	}

	return nil
}

func replaceDirWithRetry(src, dst string, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := replaceDir(src, dst); err != nil {
			lastErr = err
		} else {
			return nil
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(interval)
	}
}

func replaceDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return copyDir(src, dst)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		case entry.Type().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return nil
		}
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
