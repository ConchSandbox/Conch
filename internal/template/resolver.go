package template

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/vmm"
)

type RecordStore interface {
	Get(context.Context, string) (state.TemplateRecord, error)
}

type SnapshotBackend interface {
	CreateBootLayout(ctx context.Context, namespace, key string, req snapshot.BootLayoutRequest) (*snapshot.BootLayout, error)
	RestoreBootLayout(ctx context.Context, namespace, key string, req snapshot.BootLayoutRequest) (*snapshot.BootLayout, error)
	ReleaseBootLayout(ctx context.Context, namespace, key string) error
}

// BootIndexBackend validates and unpacks immutable Boot Index content into
// the local committed snapshots used as per-sandbox runtime parents.
type BootIndexBackend interface {
	ResolveBootIndex(ctx context.Context, namespace, bootIndexDigest string) (conchimage.ResolveBootIndexResult, error)
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
	BootIndexDigest string
	CapturedVMMName string
	RootfsKey       string
	MemKey          string
	RootfsMount     string
	MemMount        string
	VMMount         string
	RootDir         string
	MemSize         int64
	Resume          bool
}

type PrepareSandboxBootRequest struct {
	Namespace  string
	TemplateID string
	SandboxID  string
	VMMName    string
	RamMB      int64
}

type PreparedSandboxBoot struct {
	Spec    SandboxBootSpec
	Runtime SandboxBootRuntime
}

type ReleaseSandboxBootRequest struct {
	Namespace string
	SandboxID string
}

type Manager struct {
	store       RecordStore
	snapshots   SnapshotBackend
	bootIndexes BootIndexBackend
}

func NewManager(store RecordStore, snapshots SnapshotBackend, bootIndexes BootIndexBackend) *Manager {
	return &Manager{
		store:       store,
		snapshots:   snapshots,
		bootIndexes: bootIndexes,
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
	resolved, err := m.resolveBootIndex(ctx, namespace, req.TemplateID, req.VMMName)
	if err != nil {
		return PreparedSandboxBoot{}, err
	}
	return m.prepareResolvedBootIndex(ctx, namespace, key, req.VMMName, req.RamMB, resolved)
}

func (m *Manager) prepareResolvedBootIndex(
	ctx context.Context,
	namespace, key string,
	requestedVMM string,
	ramMB int64,
	resolved conchimage.ResolveBootIndexResult,
) (PreparedSandboxBoot, error) {
	parents := snapshot.ParentSnapshotIDs{
		Rootfs: strings.TrimSpace(resolved.RootfsKey),
		Mem:    strings.TrimSpace(resolved.MemKey),
		VM:     strings.TrimSpace(resolved.VMKey),
	}
	resume := resolved.Resume
	bootVMM := strings.TrimSpace(requestedVMM)
	if resume {
		bootVMM = strings.TrimSpace(resolved.VMMName)
	}
	memoryLayout, err := memoryLayoutForVMM(bootVMM, resume)
	if err != nil {
		return PreparedSandboxBoot{}, err
	}
	memorySizeMB := ramMB
	if resume {
		memorySizeMB = resolved.MemorySizeMB
	}
	layoutReq := snapshot.BootLayoutRequest{
		Parents:      parents,
		MemoryLayout: memoryLayout,
		MemorySizeMB: memorySizeMB,
	}
	var layout *snapshot.BootLayout
	if resume {
		layout, err = m.snapshots.RestoreBootLayout(ctx, namespace, key, layoutReq)
	} else {
		layout, err = m.snapshots.CreateBootLayout(ctx, namespace, key, layoutReq)
	}
	if err != nil {
		return PreparedSandboxBoot{}, fmt.Errorf("failed to prepare boot layout: %w", err)
	}
	runtimeMemKey := ""
	if strings.TrimSpace(layout.MemMount) != "" {
		runtimeMemKey = snapshot.MemKeyFromRootfs(key)
	}
	return PreparedSandboxBoot{
		Spec: bootSpecFromLayout(layout),
		Runtime: SandboxBootRuntime{
			BootIndexDigest: resolved.BootIndexDigest,
			CapturedVMMName: resolved.VMMName,
			RootfsKey:       key,
			MemKey:          runtimeMemKey,
			RootfsMount:     layout.RootfsMount,
			MemMount:        layout.MemMount,
			VMMount:         layout.VMMount,
			RootDir:         layout.SnapshotDir,
			MemSize:         layout.MemorySizeMB,
			Resume:          resume,
		},
	}, nil
}

func memoryLayoutForVMM(vmmName string, resume bool) (snapshot.MemoryLayoutMode, error) {
	switch strings.TrimSpace(vmmName) {
	case vmm.CloudHypervisorName:
		return snapshot.MemoryLayoutWritableFile, nil
	case vmm.StratovirtName:
		if resume {
			return snapshot.MemoryLayoutCheckpointView, nil
		}
		return snapshot.MemoryLayoutNone, nil
	default:
		return "", fmt.Errorf("unsupported VMM %q for boot layout", vmmName)
	}
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

func (m *Manager) resolveBootIndex(ctx context.Context, namespace, templateID, requestedVMM string) (conchimage.ResolveBootIndexResult, error) {
	if m == nil || m.store == nil {
		return conchimage.ResolveBootIndexResult{}, fmt.Errorf("template record store is not configured")
	}
	if m.bootIndexes == nil {
		return conchimage.ResolveBootIndexResult{}, fmt.Errorf("boot index resolver is not configured")
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return conchimage.ResolveBootIndexResult{}, fmt.Errorf("template_id is required")
	}
	rec, err := m.store.Get(ctx, templateID)
	if err != nil {
		return conchimage.ResolveBootIndexResult{}, err
	}
	if rec.State != state.TemplateReady {
		return conchimage.ResolveBootIndexResult{}, fmt.Errorf("template %s is %s, want %s", rec.ID, rec.State, state.TemplateReady)
	}
	recordNamespace := normalizeNamespace(rec.Namespace)
	if namespace != recordNamespace {
		return conchimage.ResolveBootIndexResult{}, fmt.Errorf("template %s belongs to namespace %s, not %s", rec.ID, recordNamespace, namespace)
	}
	bootIndexDigest := strings.TrimSpace(rec.BootIndexDigest)
	if bootIndexDigest == "" {
		return conchimage.ResolveBootIndexResult{}, fmt.Errorf("template %s has no boot index digest", rec.ID)
	}
	resolved, err := m.bootIndexes.ResolveBootIndex(ctx, recordNamespace, bootIndexDigest)
	if err != nil {
		return conchimage.ResolveBootIndexResult{}, fmt.Errorf("resolve template %s boot index %s: %w", rec.ID, bootIndexDigest, err)
	}
	if resolved.BootIndexDigest != bootIndexDigest {
		return conchimage.ResolveBootIndexResult{}, fmt.Errorf("resolved boot index digest %s does not match template digest %s", resolved.BootIndexDigest, bootIndexDigest)
	}
	if err := validateResolvedBootIndex(resolved, BootMode(rec), strings.TrimSpace(requestedVMM)); err != nil {
		return conchimage.ResolveBootIndexResult{}, fmt.Errorf("template %s: %w", rec.ID, err)
	}
	return resolved, nil
}

func validateResolvedBootIndex(resolved conchimage.ResolveBootIndexResult, expectedMode, requestedVMM string) error {
	if strings.TrimSpace(resolved.RootfsKey) == "" || strings.TrimSpace(resolved.VMKey) == "" {
		return fmt.Errorf("boot index unpack returned incomplete parents")
	}
	resolvedMode := state.TemplateBootModeCold
	if resolved.Resume {
		resolvedMode = state.TemplateBootModeResume
		if strings.TrimSpace(resolved.MemKey) == "" {
			return fmt.Errorf("resume boot index unpack returned empty mem parent")
		}
		if requestedVMM != "" && strings.TrimSpace(resolved.VMMName) != requestedVMM {
			return fmt.Errorf("boot index was captured by VMM %s, not %s", resolved.VMMName, requestedVMM)
		}
		if resolved.MemorySizeMB < 0 {
			return fmt.Errorf("resume boot index has invalid memory size %d MB", resolved.MemorySizeMB)
		}
		if strings.TrimSpace(resolved.VMMName) == vmm.StratovirtName && resolved.MemorySizeMB == 0 {
			return fmt.Errorf("StratoVirt resume boot index is missing memory size metadata")
		}
	} else if strings.TrimSpace(resolved.MemKey) != "" {
		return fmt.Errorf("cold boot index unpack returned an unexpected mem parent")
	}
	if expectedMode != state.TemplateBootModeCold && expectedMode != state.TemplateBootModeResume {
		return fmt.Errorf("unknown expected boot mode %q", expectedMode)
	}
	if expectedMode != resolvedMode {
		return fmt.Errorf("cached boot mode %s does not match Boot Index capability %s", expectedMode, resolvedMode)
	}
	return nil
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
	memoryPath := ""
	snapfilePath := ""
	if strings.TrimSpace(runtime.MemMount) != "" {
		memoryPath = filepath.Join(runtime.MemMount, common.MemFileName)
		snapfilePath = filepath.Join(runtime.MemMount, strings.TrimLeft(rootDir, string(filepath.Separator)))
	}
	return SandboxBootSpec{
		MemorySizeMB: memSize,
		MemoryPath:   memoryPath,
		KernelPath:   filepath.Join(runtime.VMMount, common.VmKernelRelativePath),
		InitrdPath:   filepath.Join(runtime.VMMount, common.VmInitrdRelativePath),
		SnapfilePath: snapfilePath,
	}
}
