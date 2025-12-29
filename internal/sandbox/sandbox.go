package sandbox

import (
	"context"
	"errors"
	"fmt"

	"conch/core/sandbox/network"
	"conch/core/sandbox/vmm"
	"conch/core/snapshot"
)

type Execution struct {
	Logs string `json:"logs"`
}

type Sandbox struct {
	cleanup      *Cleanup
	process      *vmm.Process
	snapshotConf *snapshot.SnapshotConfig
	namespace    string
	slot         *network.Slot
}

func ResumeSandbox(
	ctx context.Context,
	snapshotConf *snapshot.SnapshotConfig,
	vmmName, sandboxId, kernelPath string,
	DiskPath string, pool *network.Pool,
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
		return nil, fmt.Errorf("failed to init network")
	}

	fmt.Printf("get slot %s\n", slot.Key)

	cleanup.Add(func(ctx context.Context) error {
		err := pool.Release(ctx, slot)
		if err != nil {
			return fmt.Errorf("failed to release network slot %s: %w", slot.Key, err)
		}
		return nil
	})

	namespaceID := slot.NamespaceID()
	tapName := slot.TapName()
	rootfs := snapshotConf.Rootfs
	rootfsSock := snapshotConf.RootfsSock
	memSize := snapshotConf.MemSize
	memFile := snapshotConf.SnapshotMemFile()

	vmmHandle, vmmErr := vmm.NewProcess(
		rootfs, memFile,
		rootfsSock, kernelPath, DiskPath,
		vmmName, sandboxId,
		namespaceID, tapName,
		memSize, true,
	)
	if vmmErr != nil {
		return nil, fmt.Errorf("failed to init VMM: %w", vmmErr)
	}

	snapfilePath := snapshotConf.FullRootDir()
	err = vmmHandle.Resume(ctx, snapfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create VMM: %w", err)
	}

	sbx := &Sandbox{
		snapshotConf: snapshotConf,
		process:      vmmHandle,
		cleanup:      cleanup,
		slot:         slot,
	}

	cleanup.Add(func(ctx context.Context) error {
		filesErr := cleanupFiles(rootfsSock, sbx.process.VmmSocketPath)
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
	vmmName, sandboxId, kernelPath string,
	DiskPath string, pool *network.Pool,
) (s *Sandbox, e error) {
	// debug
	fmt.Printf("Creating sandbox: vmmName %s, sandboxId %s, kernelPath %s...\n",
		vmmName, sandboxId, kernelPath)

	cleanup := NewCleanup()
	defer func() {
		if e != nil {
			cleanupErr := cleanup.Run(ctx)
			e = errors.Join(e, cleanupErr)
		}
	}()

	slot, err := pool.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to init network")
	}

	fmt.Printf("get slot %s\n", slot.Key)

	cleanup.Add(func(ctx context.Context) error {
		err := pool.Release(ctx, slot)
		if err != nil {
			return fmt.Errorf("failed to release network slot %s: %w", slot.Key, err)
		}
		return nil
	})
	namespaceID := slot.NamespaceID()
	tapName := slot.TapName()

	rootfs := snapshotConf.Rootfs
	rootfsSock := snapshotConf.RootfsSock
	memSize := snapshotConf.MemSize
	memFile := snapshotConf.SnapshotMemFile()

	vmmHandle, vmmErr := vmm.NewProcess(
		rootfs, memFile,
		rootfsSock, kernelPath, DiskPath,
		vmmName, sandboxId,
		namespaceID, tapName,
		memSize, false,
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
		slot:         slot,
	}

	cleanup.Add(func(ctx context.Context) error {
		filesErr := cleanupFiles(rootfsSock, sbx.process.VmmSocketPath)
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
	if err := s.process.Pause(ctx); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}

	err := s.process.CreateSnapshot(ctx, s.snapshotConf.FullRootDir())
	if err != nil {
		return fmt.Errorf("error creating snapshot: %w", err)
	}

	return nil
}