package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	"conch/internal/sandbox/network"
	"conch/internal/snapshot"
)

const (
	requestTimeout = 60 * time.Second
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
	SnapshotId string `json:"snapshot_id"`
}

func (m *Manager) Create(req SandboxCreateRequest) (string, error) {
	// debug
	fmt.Println("Creating sandbox in manager...")

	ctx, cancel := context.WithTimeoutCause(context.Background(), requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	var sbx *Sandbox
	var slotKey string
	var namespace = "default"
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

		sbx, slotKey, err = ResumeSandbox(ctx, snapshotConf, req.VmmName, req.SandboxId, req.KernelPath,req.DiskPath, m.pool)
		if err != nil {
			return "", fmt.Errorf("failed to create sandbox: %w", err)
		}
	} else {
		// debug
		fmt.Printf("Creating sandbox by ImageId %s\n", req.ImageId)
		var key = req.SandboxId
		// TODO: Image to Snapshot
		// parent, err := image.GetSnapshot(imageId)
		var parent = req.ImageId
		// var parent = "sha256:9864188ae7e73d7d0e5e4f52441721380a1564c262a0fbf5795a594c281bf737"
		// var parent = "sha256:40ceed822137bb5130aa99f5bf8162633206a588573bebd64b2e1d14fcdd77de" // sh image
		// var parent = "sha256:6a58dd84acaf6438346ea907a1131f15e8d085e576f6203415e43d067cf174a3" // systemd ubuntu

		// TODO: delete this code "Remove" when failed clean code ready
		snapshot.Remove(context.Background(), namespace, key)

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

		sbx, slotKey, err = CreateSandbox(ctx, snapshotConf, req.VmmName, req.SandboxId, req.KernelPath, req.DiskPath, m.pool)
		if err != nil {
			return "", fmt.Errorf("failed to create sandbox: %w", err)
		}
	}

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

	return slotKey, nil
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
		var namespace = "default"
		err = snapshot.Remove(context.Background(), namespace, req.SandboxId)
		if err != nil {
			fmt.Printf("sandbox %s Remove error: %v\n", req.SandboxId, err)
		}
	}()
	return nil
}

func (m *Manager) Pause(req SandboxPauseRequest) error {
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
	defer func() {
		fmt.Printf("sandbox %s stop in pause\n", req.SandboxId)
		if err := sbx.Stop(ctx); err != nil {
			fmt.Printf("sandbox %s stop error after pause: %v\n", req.SandboxId, err)
		}
		if err := sbx.Close(ctx); err != nil {
			fmt.Printf("sandbox %s close error after pause: %v\n", req.SandboxId, err)
		}
		var namespace = "default"
		if err := snapshot.Remove(context.Background(), namespace, req.SandboxId); err != nil {
			fmt.Printf("sandbox %s Remove error after pause: %v\n", req.SandboxId, err)
		}
	}()

	if err := sbx.Pause(ctx); err != nil {
		return fmt.Errorf("sandbox %s pause failed: %w", req.SandboxId, err)
	}

	// TODO: system sync, too large
	syscall.Sync()

	// pause create need new name
	var name = req.SnapshotId
	var key = req.SandboxId
	var namespace = "default"
	err := snapshot.Commit(context.Background(), namespace, name, key)
	if err != nil {
		return fmt.Errorf("error adding snapshot %s : %v", req.SandboxId, err)
	}
	return nil
}

func (m *Manager) RunCode(code string) map[string]interface{} {
	fmt.Println("Starting virtual machine...")
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("Executing code: %s\n", code)

	logs := executeCode(code)

	fmt.Println("Destroying virtual machine...")
	time.Sleep(50 * time.Millisecond)

	return map[string]interface{}{
		"logs": logs,
	}
}

func executeCode(code string) string {
	if strings.Contains(code, "print(") {
		contentStart := strings.Index(code, "('") + 2
		contentEnd := strings.Index(code, "')")
		if contentEnd > contentStart {
			content := code[contentStart:contentEnd]
			return fmt.Sprintf("From Sandbox: %s", content)
		}
	}
	return "Code executed successfully (simulated)"
}