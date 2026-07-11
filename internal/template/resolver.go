package template

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/snapshots"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
)

type RecordStore interface {
	Get(context.Context, string) (state.TemplateRecord, error)
}

type SnapshotBackend interface {
	CreateBootLayout(ctx context.Context, namespace, key string, parents snapshot.ParentSnapshotIDs, memorySizeMB int64) (*snapshot.BootLayout, error)
	RestoreBootLayout(ctx context.Context, namespace, key string, parents snapshot.ParentSnapshotIDs, cid uint32, socketPath string) (*snapshot.BootLayout, error)
	ReleaseBootLayout(ctx context.Context, namespace, key string) error
	SnapshotInfo(ctx context.Context, namespace, key string) (snapshots.Info, error)
	CommitBootLayout(ctx context.Context, namespace, snapshotID, key, capturePath, parentVMID string) (string, error)
}

type SandboxBootSpec struct {
	MemorySizeMB int64

	MemoryPath   string
	KernelPath   string
	InitrdPath   string
	SnapfilePath string
	PmemPaths    []string
}

type SandboxBootRuntime struct {
	RootfsKey      string
	MemKey         string
	ParentRootfsID string
	ParentMemID    string
	ParentVMID     string
	RootfsMount    string
	MemMount       string
	VMMount        string
	RootDir        string
	MemSize        int64
	Resume         bool
}

type PrepareSandboxBootRequest struct {
	Namespace       string
	TemplateID      string
	SandboxID       string
	RamMB           int64
	VsockCID        uint32
	VsockSocketPath string
}

type PreparedSandboxBoot struct {
	Spec    SandboxBootSpec
	Runtime SandboxBootRuntime
}

type ReleaseSandboxBootRequest struct {
	Namespace string
	SandboxID string
}

type CommitSandboxBootRequest struct {
	Namespace   string
	SandboxID   string
	TemplateID  string
	CapturePath string
	ParentVMID  string
}

type SandboxBootCommit struct {
	RootfsKey string
	MemKey    string
	VMKey     string
}

type Manager struct {
	store     RecordStore
	snapshots SnapshotBackend
}

func NewManager(store RecordStore, snapshots SnapshotBackend) *Manager {
	return &Manager{
		store:     store,
		snapshots: snapshots,
	}
}

func (m *Manager) PrepareSandboxBoot(ctx context.Context, req PrepareSandboxBootRequest) (PreparedSandboxBoot, error) {
	if m == nil || m.snapshots == nil {
		return PreparedSandboxBoot{}, fmt.Errorf("template boot manager is not configured")
	}
	namespace := normalizeNamespace(req.Namespace)
	key := strings.TrimSpace(req.SandboxID)
	if key == "" {
		return PreparedSandboxBoot{}, fmt.Errorf("sandbox_id is required")
	}
	parents, err := m.resolveParents(ctx, req.TemplateID)
	if err != nil {
		return PreparedSandboxBoot{}, err
	}
	resume := canRestore(parents)
	var layout *snapshot.BootLayout
	if resume {
		layout, err = m.snapshots.RestoreBootLayout(ctx, namespace, key, parents, req.VsockCID, strings.TrimSpace(req.VsockSocketPath))
	} else {
		layout, err = m.snapshots.CreateBootLayout(ctx, namespace, key, parents, req.RamMB)
	}
	if err != nil {
		return PreparedSandboxBoot{}, fmt.Errorf("failed to prepare boot layout: %w", err)
	}
	return PreparedSandboxBoot{
		Spec: bootSpecFromLayout(layout),
		Runtime: SandboxBootRuntime{
			RootfsKey:      key,
			MemKey:         snapshot.MemKeyFromRootfs(key),
			ParentRootfsID: parents.Rootfs,
			ParentMemID:    parents.Mem,
			ParentVMID:     parents.VM,
			RootfsMount:    layout.RootfsMount,
			MemMount:       layout.MemMount,
			VMMount:        layout.VMMount,
			RootDir:        layout.SnapshotDir,
			MemSize:        layout.MemorySizeMB,
			Resume:         resume,
		},
	}, nil
}

func (m *Manager) ReleaseSandboxBoot(ctx context.Context, req ReleaseSandboxBootRequest) error {
	if m == nil || m.snapshots == nil {
		return fmt.Errorf("template boot manager is not configured")
	}
	namespace := normalizeNamespace(req.Namespace)
	key := strings.TrimSpace(req.SandboxID)
	if key == "" {
		return fmt.Errorf("sandbox_id is required")
	}
	return m.snapshots.ReleaseBootLayout(ctx, namespace, key)
}

func (m *Manager) CommitSandboxBoot(ctx context.Context, req CommitSandboxBootRequest) (SandboxBootCommit, error) {
	if m == nil || m.snapshots == nil {
		return SandboxBootCommit{}, fmt.Errorf("template boot manager is not configured")
	}
	namespace := normalizeNamespace(req.Namespace)
	key := strings.TrimSpace(req.SandboxID)
	if key == "" {
		return SandboxBootCommit{}, fmt.Errorf("sandbox_id is required")
	}
	templateID := strings.TrimSpace(req.TemplateID)
	if templateID == "" {
		return SandboxBootCommit{}, fmt.Errorf("template_id is required")
	}
	capturePath := strings.TrimSpace(req.CapturePath)
	if capturePath == "" {
		return SandboxBootCommit{}, fmt.Errorf("capture_path is required")
	}
	parentVMID := strings.TrimSpace(req.ParentVMID)
	if parentVMID == "" {
		return SandboxBootCommit{}, fmt.Errorf("parent_vm_id is required")
	}
	info, err := m.snapshots.SnapshotInfo(ctx, namespace, key)
	if err != nil {
		return SandboxBootCommit{}, fmt.Errorf("failed to get snapshot info %s: %w", key, err)
	}
	snapshotID, err := snapshot.CalculateSnapshotID(namespace, templateID, info.Parent)
	if err != nil {
		return SandboxBootCommit{}, fmt.Errorf("failed to calculate snapshot id: %w", err)
	}
	memSnapshotID, err := snapshot.CalculateSnapshotID(namespace, snapshotID+common.MemKeySuffix, "")
	if err != nil {
		return SandboxBootCommit{}, fmt.Errorf("failed to calculate mem snapshot id: %w", err)
	}
	snapshotID, err = m.snapshots.CommitBootLayout(ctx, namespace, snapshotID, key, capturePath, parentVMID)
	if err != nil {
		return SandboxBootCommit{}, fmt.Errorf("failed to commit boot layout %s: %w", key, err)
	}
	return SandboxBootCommit{
		RootfsKey: snapshotID,
		MemKey:    memSnapshotID,
		VMKey:     parentVMID,
	}, nil
}

func (m *Manager) resolveParents(ctx context.Context, templateID string) (snapshot.ParentSnapshotIDs, error) {
	if m == nil || m.store == nil {
		return snapshot.ParentSnapshotIDs{}, fmt.Errorf("template record store is not configured")
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return snapshot.ParentSnapshotIDs{}, fmt.Errorf("template_id is required")
	}
	rec, err := m.store.Get(ctx, templateID)
	if err != nil {
		return snapshot.ParentSnapshotIDs{}, err
	}
	if rec.State != state.TemplateReady {
		return snapshot.ParentSnapshotIDs{}, fmt.Errorf("template %s is %s, want %s", rec.ID, rec.State, state.TemplateReady)
	}
	parents := snapshot.ParentSnapshotIDs{
		Rootfs: strings.TrimSpace(rec.RootfsKey),
		Mem:    strings.TrimSpace(rec.MemKey),
		VM:     strings.TrimSpace(rec.VMKey),
	}
	if parents.Rootfs == "" {
		return snapshot.ParentSnapshotIDs{}, fmt.Errorf("template %s rootfs key is empty", rec.ID)
	}
	if parents.VM == "" {
		return snapshot.ParentSnapshotIDs{}, fmt.Errorf("template %s vm key is empty", rec.ID)
	}
	if m.snapshots != nil {
		for _, ref := range []struct {
			kind string
			key  string
		}{
			{kind: "rootfs", key: parents.Rootfs},
			{kind: "mem", key: parents.Mem},
			{kind: "vm", key: parents.VM},
		} {
			if ref.key == "" {
				continue
			}
			if _, err := m.snapshots.SnapshotInfo(ctx, rec.Namespace, ref.key); err != nil {
				return snapshot.ParentSnapshotIDs{}, fmt.Errorf("template %s %s snapshot %s is unavailable: %w", rec.ID, ref.kind, ref.key, err)
			}
		}
	}
	return parents, nil
}

func canRestore(parents snapshot.ParentSnapshotIDs) bool {
	return strings.TrimSpace(parents.Rootfs) != "" &&
		strings.TrimSpace(parents.Mem) != "" &&
		strings.TrimSpace(parents.VM) != ""
}

func bootSpecFromLayout(layout *snapshot.BootLayout) SandboxBootSpec {
	if layout == nil {
		return SandboxBootSpec{}
	}
	return SandboxBootSpec{
		MemorySizeMB: layout.MemorySizeMB,
		MemoryPath:   layout.SnapshotMemFile(),
		KernelPath:   layout.KernelFile(),
		InitrdPath:   layout.InitrdFile(),
		SnapfilePath: layout.SnapDir(),
		PmemPaths:    layout.PmemFiles(),
	}
}

func BootSpecFromRuntime(runtime SandboxBootRuntime) SandboxBootSpec {
	rootDir := runtime.RootDir
	if rootDir == "" {
		rootDir = "conch/snapshot"
	}
	memSize := runtime.MemSize
	if memSize <= 0 {
		memSize = common.MemFileDefaultSize
	}
	return SandboxBootSpec{
		MemorySizeMB: memSize,
		MemoryPath:   filepath.Join(runtime.MemMount, common.MemFileName),
		KernelPath:   filepath.Join(runtime.VMMount, common.VmKernelRelativePath),
		InitrdPath:   filepath.Join(runtime.VMMount, common.VmInitrdRelativePath),
		SnapfilePath: filepath.Join(runtime.MemMount, strings.TrimLeft(rootDir, string(filepath.Separator))),
	}
}
