package sandbox

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/openeuler/Conch/internal/sandbox/network"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/utils"
)

const (
	requestTimeout   = 60 * time.Second
	defaultNamespace = "default"
)

type Manager struct {
	sandboxes sync.Map
	pool      *network.Pool
}

func NewManager(p *network.Pool) *Manager {
	return &Manager{
		pool: p,
	}
}

type SandboxCreateRequest struct {
	SnapshotId string `json:"snapshot_id"`
	ImageId    string `json:"image_id"`
	VmmName    string `json:"vmm_name"`
	SandboxId  string `json:"sandbox_id"`
	KernelPath string `json:"kernel_path"`
	DiskPath   string `json:"disk_image_path"`
	VcpuNum    int64  `json:"vcpu_num"`
	RamMB      int64  `json:"ram_mb"`
}

type SandboxDeleteRequest struct {
	SandboxId string `json:"sandbox_id"`
}

type SandboxPauseRequest struct {
	SandboxId string `json:"sandbox_id"`
}

func (m *Manager) Create(req SandboxCreateRequest) (string, error) {
	// debug
	fmt.Println("Creating sandbox in manager...")

	ctx, cancel := context.WithTimeoutCause(context.Background(), requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	var sbx *Sandbox
	var peerIP string
	var namespace = defaultNamespace
	if req.SnapshotId != "" {
		// debug
		fmt.Println("Creating sandbox by snapshotId...")

		var key = req.SandboxId
		var parent = req.SnapshotId // snapshot.Commit name
		snapshotConf, err := snapshot.Prepare(context.Background(), namespace, key, parent,
			func(info *snapshot.SnapshotConfig) error {
				info.MemSize = req.RamMB
				// info.RootDir = "/tmp"
				return nil
			})
		if err != nil {
			return "", fmt.Errorf("failed to prepare snapshot: %w", err)
		}
		defer func() {
			if err != nil {
				err = snapshot.Remove(context.Background(), namespace, key)
				if err != nil {
					fmt.Printf("failed to remove snapshot: %v", err)
				}
				fmt.Printf("removed snapshot: %s\n", key)
			}
		}()

		sbx, err = ResumeSandbox(ctx, snapshotConf, req.VmmName, req.SandboxId, req.KernelPath, req.DiskPath, m.pool)
		if err != nil {
			return "", fmt.Errorf("failed to create sandbox: %w", err)
		}
	} else {
		// debug
		fmt.Printf("Creating sandbox by ImageId %s\n", req.ImageId)
		var key = req.SandboxId
		// Temp: ImageId is SnapshotId now
		// TODO: replace with image.GetSnapshot
		// parent, err := image.GetSnapshot(imageId)
		var parent = req.ImageId

		snapshotConf, err := snapshot.Prepare(context.Background(), namespace, key, parent,
			func(info *snapshot.SnapshotConfig) error {
				info.MemSize = req.RamMB
				// info.Rootfs = "/tmp"
				return nil
			})
		if err != nil {
			return "", fmt.Errorf("failed to prepare snapshot: %w", err)
		}
		defer func() {
			if err != nil {
				err = snapshot.Remove(context.Background(), namespace, key)
				if err != nil {
					fmt.Printf("failed to remove snapshot: %v", err)
				}
				fmt.Printf("removed snapshot: %s\n", key)
			}
		}()

		sbx, err = CreateSandbox(ctx, snapshotConf, req.VmmName, req.SandboxId, req.KernelPath, req.DiskPath, m.pool)
		if err != nil {
			return "", fmt.Errorf("failed to create sandbox: %w", err)
		}
	}

	peerIP = sbx.slot.VpeerIPString()

	m.sandboxes.Store(req.SandboxId, sbx)
	go func() {
		waitErr := sbx.Wait(ctx)
		if waitErr != nil {
			fmt.Printf("failed to wait for sandbox, %s, cleaning up\n", waitErr)
		}

		cleanupErr := sbx.Close(ctx)
		if cleanupErr != nil {
			fmt.Printf("failed to cleanup sandbox, %s, will remove from cache\n", cleanupErr)
		}

		snapshot.Remove(context.Background(), namespace, req.SandboxId)

		m.sandboxes.Delete(req.SandboxId)

		// fmt.Printf("Sandbox %s killed\n", req.SandboxId)
	}()

	//debug
	fmt.Println("Created sandbox in manager...")

	return peerIP, nil
}

func (m *Manager) Delete(req SandboxDeleteRequest) error {
	ctx, cancel := context.WithTimeoutCause(context.Background(), requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	sbxVal, exists := m.sandboxes.Load(req.SandboxId)
	if !exists {
		return fmt.Errorf("sandbox %s not found", req.SandboxId)
	}

	sbx, ok := sbxVal.(*Sandbox)
	if !ok {
		return fmt.Errorf("invalid sandbox type for %s", req.SandboxId)
	}

	m.sandboxes.Delete(req.SandboxId)
	go func() {
		err := sbx.Stop(ctx)
		if err != nil {
			fmt.Printf("sandbox %s stop error: %v\n", req.SandboxId, err)
		}
		var namespace = defaultNamespace
		err = snapshot.Remove(context.Background(), namespace, req.SandboxId)
		if err != nil {
			fmt.Printf("sandbox %s Remove error: %v\n", req.SandboxId, err)
		}
	}()
	return nil
}

func (m *Manager) Pause(req SandboxPauseRequest) (string, error) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	sbxVal, exists := m.sandboxes.Load(req.SandboxId)
	if !exists {
		return "", fmt.Errorf("sandbox %s not found", req.SandboxId)
	}

	sbx, ok := sbxVal.(*Sandbox)
	if !ok {
		return "", fmt.Errorf("invalid sandbox type for %s", req.SandboxId)
	}

	m.sandboxes.Delete(req.SandboxId)
	defer func() {
		fmt.Printf("sandbox %s stop in pause\n", req.SandboxId)
		if err := sbx.Stop(ctx); err != nil {
			fmt.Printf("sandbox %s stop error after pause: %v\n", req.SandboxId, err)
		}
		if err := sbx.Close(ctx); err != nil {
			fmt.Printf("sandbox %s close error after pause: %v\n", req.SandboxId, err)
		}
		var namespace = defaultNamespace
		if err := snapshot.Remove(context.Background(), namespace, req.SandboxId); err != nil {
			fmt.Printf("sandbox %s Remove error after pause: %v\n", req.SandboxId, err)
		}
	}()

	if err := sbx.Pause(ctx); err != nil {
		return "", fmt.Errorf("sandbox %s pause failed: %w", req.SandboxId, err)
	}

	// TODO: system sync, too large
	syscall.Sync()

	var key = req.SandboxId
	var namespace = defaultNamespace

	info, err := snapshot.Stat(ctx, namespace, key)
	if err != nil {
		return "", fmt.Errorf("failed to stat snapshot %s: %w", key, err)
	}
	parent := info.Parent
	snapshotId, err := utils.CalculateSnapshotName(namespace, key, parent)
	if err != nil {
		return "", fmt.Errorf("failed to calculate snapshot name: %w", err)
	}

	err = snapshot.Commit(context.Background(), namespace, snapshotId, key)
	if err != nil {
		return "", fmt.Errorf("error committing snapshot %s: %v", req.SandboxId, err)
	}

	return snapshotId, nil
}

func (m *Manager) CleanupPool() error {
	// debug
	fmt.Println("cleanup pool begin, wait for a few seconds")
	err := m.pool.Cleanup()
	if err != nil {
		return fmt.Errorf("failed to cleanup pool: %v", err)
	}
	fmt.Println("cleanup pool finish")

	return nil
}
