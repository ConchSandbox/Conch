package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/memorymode"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/internal/vmm"
)

type TemplateReader interface {
	Get(context.Context, string) (template.Entry, error)
}

type SnapshotBackend interface {
	CreateBootLayout(ctx context.Context, key string, req snapshot.BootLayoutRequest) (*snapshot.BootLayout, error)
	RestoreBootLayout(ctx context.Context, key string, req snapshot.BootLayoutRequest) (*snapshot.BootLayout, error)
	ReleaseBootLayout(ctx context.Context, key string) error
}

type BootSpec struct {
	MemorySizeMB int64
	MemoryFormat string

	MemoryPath   string
	KernelPath   string
	InitrdPath   string
	SnapfilePath string
	PmemPaths    []string
}

type BootRuntime struct {
	BootIndexDigest    string
	CapturedVMMName    string
	RootfsKey          string
	MemKey             string
	RootfsMount        string
	MemorySnapshotRoot string
	VMMount            string
	MemSize            int64
	MemoryFormat       string
	Resume             bool
}

type BootPreflightRequest struct {
	TemplateID          string
	VMMName             string
	RequestedMemoryMode string
}

type BootPlan struct {
	entry        template.Entry
	info         conchimage.BootIndexInfo
	requestedVMM string
	memoryMode   memorymode.Mode
}

type PrepareBootRequest struct {
	Plan      BootPlan
	SandboxID string
	RAMMB     int64
}

type PreparedBoot struct {
	Spec       BootSpec
	Runtime    BootRuntime
	MemoryMode memorymode.Mode
}

type ReleaseBootRequest struct {
	SandboxID string
}

type BootPreparer interface {
	Preflight(context.Context, BootPreflightRequest) (BootPlan, error)
	Prepare(context.Context, PrepareBootRequest) (PreparedBoot, error)
	Release(context.Context, ReleaseBootRequest) error
}

type bootPreparer struct {
	templates           TemplateReader
	snapshots           SnapshotBackend
	inspectBoot         func(context.Context, string) (conchimage.BootIndexInfo, error)
	resolveBootFromInfo func(context.Context, conchimage.BootIndexInfo) (conchimage.ResolvedBoot, error)
}

func NewBootPreparer(templates TemplateReader, snapshots SnapshotBackend, client *containerdclient.Client) (BootPreparer, error) {
	if client == nil || client.Client == nil {
		return nil, fmt.Errorf("containerd client is required")
	}
	return newBootPreparer(
		templates,
		snapshots,
		func(ctx context.Context, bootIndexDigest string) (conchimage.BootIndexInfo, error) {
			return conchimage.InspectBootIndex(ctx, client, bootIndexDigest)
		},
		func(ctx context.Context, info conchimage.BootIndexInfo) (conchimage.ResolvedBoot, error) {
			return conchimage.ResolveBootFromInfo(ctx, client, info)
		},
	)
}

func newBootPreparer(
	templates TemplateReader,
	snapshots SnapshotBackend,
	inspectBoot func(context.Context, string) (conchimage.BootIndexInfo, error),
	resolveBootFromInfo func(context.Context, conchimage.BootIndexInfo) (conchimage.ResolvedBoot, error),
) (BootPreparer, error) {
	if templates == nil {
		return nil, fmt.Errorf("template reader is required")
	}
	if snapshots == nil {
		return nil, fmt.Errorf("snapshot backend is required")
	}
	if inspectBoot == nil {
		return nil, fmt.Errorf("boot index inspector is required")
	}
	if resolveBootFromInfo == nil {
		return nil, fmt.Errorf("boot resolver is required")
	}
	return &bootPreparer{
		templates:           templates,
		snapshots:           snapshots,
		inspectBoot:         inspectBoot,
		resolveBootFromInfo: resolveBootFromInfo,
	}, nil
}

func (p *bootPreparer) Preflight(ctx context.Context, req BootPreflightRequest) (BootPlan, error) {
	if p == nil || p.templates == nil || p.snapshots == nil || p.inspectBoot == nil || p.resolveBootFromInfo == nil {
		return BootPlan{}, fmt.Errorf("sandbox boot preparer is not configured")
	}
	entry, info, err := p.inspectTemplate(ctx, req.TemplateID)
	if err != nil {
		return BootPlan{}, err
	}
	requestedVMM := strings.TrimSpace(req.VMMName)
	if err := validateInspectedBoot(info, entry.BootMode, requestedVMM); err != nil {
		return BootPlan{}, fmt.Errorf("template %s: %w", entry.BootIndexDigest, err)
	}
	requestedMode := memorymode.Mode(strings.TrimSpace(req.RequestedMemoryMode))
	if requestedMode == "" {
		requestedMode = memorymode.ModeFull
	}
	bootVMM := requestedVMM
	if info.Resume {
		bootVMM = info.VMMName
	}
	err = memorymode.Validate(memorymode.Input{
		Mode:           requestedMode,
		VMMName:        bootVMM,
		Resume:         info.Resume,
		ArtifactFormat: info.MemoryFormat,
	})
	if err != nil {
		if errors.Is(err, memorymode.ErrPrecondition) {
			return BootPlan{}, ErrFailedPrecondition.Wrap(err)
		}
		return BootPlan{}, err
	}
	return BootPlan{
		entry:        entry,
		info:         info,
		requestedVMM: requestedVMM,
		memoryMode:   requestedMode,
	}, nil
}

func (p *bootPreparer) Prepare(ctx context.Context, req PrepareBootRequest) (PreparedBoot, error) {
	if p == nil || p.templates == nil || p.snapshots == nil || p.inspectBoot == nil || p.resolveBootFromInfo == nil {
		return PreparedBoot{}, fmt.Errorf("sandbox boot preparer is not configured")
	}
	key := strings.TrimSpace(req.SandboxID)
	if key == "" {
		return PreparedBoot{}, fmt.Errorf("sandbox_id is required")
	}
	plan := req.Plan
	if plan.entry.BootIndexDigest == "" || plan.info.BootIndexDigest == "" || plan.memoryMode == "" {
		return PreparedBoot{}, fmt.Errorf("boot plan is incomplete")
	}
	resolved, err := p.resolveBootFromInfo(ctx, plan.info)
	if err != nil {
		return PreparedBoot{}, err
	}
	if err := validateResolvedBoot(resolved, plan.entry.BootMode, plan.requestedVMM); err != nil {
		return PreparedBoot{}, fmt.Errorf("template %s: %w", plan.entry.BootIndexDigest, err)
	}
	prepared, err := p.prepareResolvedBoot(ctx, key, plan.requestedVMM, req.RAMMB, resolved)
	if err != nil {
		return PreparedBoot{}, err
	}
	prepared.MemoryMode = plan.memoryMode
	return prepared, nil
}

func (p *bootPreparer) inspectTemplate(
	ctx context.Context,
	id string,
) (template.Entry, conchimage.BootIndexInfo, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return template.Entry{}, conchimage.BootIndexInfo{}, fmt.Errorf("template_id is required")
	}
	entry, err := p.templates.Get(ctx, id)
	if err != nil {
		return template.Entry{}, conchimage.BootIndexInfo{}, err
	}
	bootIndexDigest := strings.TrimSpace(entry.BootIndexDigest)
	if bootIndexDigest == "" {
		return template.Entry{}, conchimage.BootIndexInfo{}, fmt.Errorf("template has no boot index digest")
	}
	info, err := p.inspectBoot(ctx, bootIndexDigest)
	if err != nil {
		return template.Entry{}, conchimage.BootIndexInfo{}, fmt.Errorf(
			"inspect template %s boot index %s: %w",
			entry.BootIndexDigest,
			bootIndexDigest,
			err,
		)
	}
	if info.BootIndexDigest != bootIndexDigest {
		return template.Entry{}, conchimage.BootIndexInfo{}, fmt.Errorf(
			"inspected boot index digest %s does not match template digest %s",
			info.BootIndexDigest,
			bootIndexDigest,
		)
	}
	return entry, info, nil
}

func (p *bootPreparer) prepareResolvedBoot(
	ctx context.Context,
	key string,
	requestedVMM string,
	ramMB int64,
	resolved conchimage.ResolvedBoot,
) (PreparedBoot, error) {
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
		return PreparedBoot{}, err
	}
	memorySizeMB := ramMB
	if resume {
		memorySizeMB = resolved.MemorySizeMB
	}
	layoutReq := snapshot.BootLayoutRequest{
		Parents:      parents,
		MemoryLayout: memoryLayout,
		MemorySizeMB: memorySizeMB,
		MemoryFormat: resolved.MemoryFormat,
	}
	var layout *snapshot.BootLayout
	if resume {
		layout, err = p.snapshots.RestoreBootLayout(ctx, key, layoutReq)
	} else {
		layout, err = p.snapshots.CreateBootLayout(ctx, key, layoutReq)
	}
	if err != nil {
		return PreparedBoot{}, fmt.Errorf("failed to prepare boot layout: %w", err)
	}
	runtimeMemKey := ""
	if strings.TrimSpace(layout.MemorySnapshotRoot) != "" {
		runtimeMemKey = snapshot.MemKeyFromRootfs(key)
	}
	return PreparedBoot{
		Spec: bootSpecFromLayout(layout),
		Runtime: BootRuntime{
			BootIndexDigest:    resolved.BootIndexDigest,
			CapturedVMMName:    resolved.VMMName,
			RootfsKey:          key,
			MemKey:             runtimeMemKey,
			RootfsMount:        layout.RootfsMount,
			MemorySnapshotRoot: layout.MemorySnapshotRoot,
			VMMount:            layout.VMMount,
			MemSize:            layout.MemorySizeMB,
			MemoryFormat:       resolved.MemoryFormat,
			Resume:             resume,
		},
	}, nil
}

func (p *bootPreparer) Release(ctx context.Context, req ReleaseBootRequest) error {
	if p == nil || p.snapshots == nil {
		return fmt.Errorf("sandbox boot preparer is not configured")
	}
	key := strings.TrimSpace(req.SandboxID)
	if key == "" {
		return fmt.Errorf("sandbox_id is required")
	}
	return p.snapshots.ReleaseBootLayout(ctx, key)
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

func validateInspectedBoot(info conchimage.BootIndexInfo, expectedMode template.BootMode, requestedVMM string) error {
	return validateBootCapability(info.Resume, info.VMMName, info.MemorySizeMB, expectedMode, requestedVMM)
}

func validateResolvedBoot(resolved conchimage.ResolvedBoot, expectedMode template.BootMode, requestedVMM string) error {
	if strings.TrimSpace(resolved.RootfsKey) == "" || strings.TrimSpace(resolved.VMKey) == "" {
		return fmt.Errorf("boot index unpack returned incomplete parents")
	}
	if resolved.Resume {
		if strings.TrimSpace(resolved.MemKey) == "" {
			return fmt.Errorf("resume boot index unpack returned empty mem parent")
		}
	} else if strings.TrimSpace(resolved.MemKey) != "" {
		return fmt.Errorf("cold boot index unpack returned an unexpected mem parent")
	}
	return validateBootCapability(resolved.Resume, resolved.VMMName, resolved.MemorySizeMB, expectedMode, requestedVMM)
}

func validateBootCapability(resume bool, capturedVMM string, memorySizeMB int64, expectedMode template.BootMode, requestedVMM string) error {
	resolvedMode := template.BootModeCold
	if resume {
		resolvedMode = template.BootModeResume
		if requestedVMM != "" && strings.TrimSpace(capturedVMM) != requestedVMM {
			return fmt.Errorf("boot index was captured by VMM %s, not %s", capturedVMM, requestedVMM)
		}
		if memorySizeMB < 0 {
			return fmt.Errorf("resume boot index has invalid memory size %d MB", memorySizeMB)
		}
		if strings.TrimSpace(capturedVMM) == vmm.StratovirtName && memorySizeMB == 0 {
			return fmt.Errorf("StratoVirt resume boot index is missing memory size metadata")
		}
	}
	if expectedMode != template.BootModeCold && expectedMode != template.BootModeResume {
		return fmt.Errorf("unknown expected boot mode %q", expectedMode)
	}
	if expectedMode != resolvedMode {
		return fmt.Errorf("cached boot mode %s does not match Boot Index capability %s", expectedMode, resolvedMode)
	}
	return nil
}

func bootSpecFromLayout(layout *snapshot.BootLayout) BootSpec {
	if layout == nil {
		return BootSpec{}
	}
	return BootSpec{
		MemorySizeMB: layout.MemorySizeMB,
		MemoryFormat: layout.MemoryFormat,
		MemoryPath:   layout.SnapshotMemFile(),
		KernelPath:   layout.KernelFile(),
		InitrdPath:   layout.InitrdFile(),
		SnapfilePath: layout.VMMStatePath(),
		PmemPaths:    layout.PmemFiles(),
	}
}

func BootSpecFromRuntime(runtime BootRuntime) BootSpec {
	memSize := runtime.MemSize
	if memSize <= 0 {
		memSize = common.MemFileDefaultSize
	}
	memoryPath := ""
	snapfilePath := ""
	if strings.TrimSpace(runtime.MemorySnapshotRoot) != "" {
		memoryPath = filepath.Join(runtime.MemorySnapshotRoot, common.MemFileName)
		snapfilePath = filepath.Join(runtime.MemorySnapshotRoot, common.VMMStateDir)
	}
	return BootSpec{
		MemorySizeMB: memSize,
		MemoryFormat: runtime.MemoryFormat,
		MemoryPath:   memoryPath,
		KernelPath:   filepath.Join(runtime.VMMount, common.VmKernelRelativePath),
		InitrdPath:   filepath.Join(runtime.VMMount, common.VmInitrdRelativePath),
		SnapfilePath: snapfilePath,
	}
}
