package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/internal/vmm"
	"github.com/openeuler/Conch/pkg/ulog"
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

	MemoryPath   string
	KernelPath   string
	InitrdPath   string
	SnapfilePath string
	PmemPaths    []string
	PreGateKey   string
	// PreGateRequired is set only when snapshot memory is still being
	// materialized. Fully local Boot Index components leave it false.
	PreGateRequired     bool
	PreGateProfile      []byte
	MaterializeCritical func(context.Context, int64, []uint64) error
	MaterializeAll      func(context.Context) error
	MaterializeCommit   func() error
}

type BootRuntime struct {
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

type PrepareBootRequest struct {
	TemplateID string
	SandboxID  string
	VMMName    string
	RAMMB      int64
}

type PreparedBoot struct {
	Spec    BootSpec
	Runtime BootRuntime
}

type ReleaseBootRequest struct {
	SandboxID string
}

type BootPreparer interface {
	Prepare(context.Context, PrepareBootRequest) (PreparedBoot, error)
	Release(context.Context, ReleaseBootRequest) error
}

type bootPreparer struct {
	templates    TemplateReader
	snapshots    SnapshotBackend
	resolveBoot  func(context.Context, string) (conchimage.ResolvedBoot, error)
	resolveLazy  func(context.Context, template.Entry) (conchimage.ResolvedBoot, error)
	resolveCache sync.Map // boot index digest -> resolvedTemplateCache
}

type resolvedTemplateCache struct {
	resolved conchimage.ResolvedBoot
	entry    template.Entry
}

func NewBootPreparer(templates TemplateReader, snapshots SnapshotBackend, client *containerdclient.Client, preGateEnabled bool, preGateStateDir string) (BootPreparer, error) {
	if client == nil || client.Client == nil {
		return nil, fmt.Errorf("containerd client is required")
	}
	preparer, err := newBootPreparer(templates, snapshots, func(ctx context.Context, bootIndexDigest string) (conchimage.ResolvedBoot, error) {
		return conchimage.ResolveBoot(ctx, client, bootIndexDigest)
	})
	if err != nil {
		return nil, err
	}
	if preGateEnabled {
		preparer.(*bootPreparer).resolveLazy = func(ctx context.Context, entry template.Entry) (conchimage.ResolvedBoot, error) {
			plainHTTP := strings.EqualFold(entry.Labels[conchimage.TemplateLabelRegistryPlainHTTP], "true")
			return conchimage.ResolveBootLazy(ctx, client, entry.BootIndexDigest, conchimage.LazyResolveOptions{
				Reference: entry.SourceRef,
				PlainHTTP: plainHTTP,
				StateDir:  preGateStateDir,
			})
		}
	}
	return preparer, nil
}

func newBootPreparer(
	templates TemplateReader,
	snapshots SnapshotBackend,
	resolveBoot func(context.Context, string) (conchimage.ResolvedBoot, error),
) (BootPreparer, error) {
	if templates == nil {
		return nil, fmt.Errorf("template reader is required")
	}
	if snapshots == nil {
		return nil, fmt.Errorf("snapshot backend is required")
	}
	if resolveBoot == nil {
		return nil, fmt.Errorf("boot resolver is required")
	}
	return &bootPreparer{
		templates:   templates,
		snapshots:   snapshots,
		resolveBoot: resolveBoot,
	}, nil
}

func (p *bootPreparer) Prepare(ctx context.Context, req PrepareBootRequest) (PreparedBoot, error) {
	if p == nil || p.templates == nil || p.snapshots == nil || p.resolveBoot == nil {
		return PreparedBoot{}, fmt.Errorf("sandbox boot preparer is not configured")
	}
	key := strings.TrimSpace(req.SandboxID)
	if key == "" {
		return PreparedBoot{}, fmt.Errorf("sandbox_id is required")
	}
	tResolve := time.Now()
	resolved, entry, err := p.resolveTemplate(ctx, req.TemplateID)
	if err != nil {
		return PreparedBoot{}, err
	}
	ulog.GetLogger().Debug("boot prep phase resolveTemplate", ulog.F("sandbox", key), ulog.F("elapsed", time.Since(tResolve)))
	if err := validateResolvedBoot(resolved, entry.BootMode, strings.TrimSpace(req.VMMName)); err != nil {
		return PreparedBoot{}, fmt.Errorf("template %s: %w", entry.BootIndexDigest, err)
	}
	tLayout := time.Now()
	prepared, err := p.prepareResolvedBoot(ctx, key, req.VMMName, req.RAMMB, resolved)
	ulog.GetLogger().Debug("boot prep phase prepareResolvedBoot", ulog.F("sandbox", key), ulog.F("elapsed", time.Since(tLayout)))
	return prepared, err
}

func (p *bootPreparer) resolveTemplate(
	ctx context.Context,
	id string,
) (conchimage.ResolvedBoot, template.Entry, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return conchimage.ResolvedBoot{}, template.Entry{}, fmt.Errorf("template_id is required")
	}
	entry, err := p.templates.Get(ctx, id)
	if err != nil {
		return conchimage.ResolvedBoot{}, template.Entry{}, err
	}
	bootIndexDigest := strings.TrimSpace(entry.BootIndexDigest)
	if bootIndexDigest == "" {
		return conchimage.ResolvedBoot{}, template.Entry{}, fmt.Errorf("template has no boot index digest")
	}
	if cached, ok := p.resolveCache.Load(bootIndexDigest); ok {
		c := cached.(resolvedTemplateCache)
		if !c.resolved.ExternalMemoryErofsPathOK() {
			p.resolveCache.Delete(bootIndexDigest)
		} else {
			return c.resolved, c.entry, nil
		}
	}
	resolved, err := p.resolveBoot(ctx, bootIndexDigest)
	if err != nil && p.resolveLazy != nil && strings.TrimSpace(entry.SourceRef) != "" {
		resolved, err = p.resolveLazy(ctx, entry)
	}
	if err != nil {
		return conchimage.ResolvedBoot{}, template.Entry{}, fmt.Errorf(
			"resolve template %s boot index %s: %w",
			entry.BootIndexDigest,
			bootIndexDigest,
			err,
		)
	}
	if resolved.BootIndexDigest != bootIndexDigest {
		return conchimage.ResolvedBoot{}, template.Entry{}, fmt.Errorf(
			"resolved boot index digest %s does not match template digest %s",
			resolved.BootIndexDigest,
			bootIndexDigest,
		)
	}
	p.resolveCache.Store(bootIndexDigest, resolvedTemplateCache{resolved: resolved, entry: entry})
	return resolved, entry, nil
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
		Parents:             parents,
		MemoryLayout:        memoryLayout,
		MemorySizeMB:        memorySizeMB,
		CheckpointErofsPath: resolved.ExternalMemoryErofsPath,
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
	if strings.TrimSpace(layout.MemMount) != "" {
		runtimeMemKey = snapshot.MemKeyFromRootfs(key)
	}
	spec := bootSpecFromLayout(layout)
	spec.PreGateKey = resolved.BootIndexDigest
	spec.PreGateRequired = resolved.PreGateRequired
	spec.PreGateProfile = append([]byte(nil), resolved.PreGateProfile...)
	spec.MaterializeCritical = resolved.MaterializeCritical
	spec.MaterializeAll = resolved.MaterializeAll
	spec.MaterializeCommit = resolved.MaterializeCommit
	return PreparedBoot{
		Spec: spec,
		Runtime: BootRuntime{
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

func validateResolvedBoot(resolved conchimage.ResolvedBoot, expectedMode template.BootMode, requestedVMM string) error {
	if resolved.PreGateRequired && !resolved.Resume {
		return fmt.Errorf("cold boot cannot require memory pre-gate materialization")
	}
	if strings.TrimSpace(resolved.RootfsKey) == "" || strings.TrimSpace(resolved.VMKey) == "" {
		return fmt.Errorf("boot index unpack returned incomplete parents")
	}
	resolvedMode := template.BootModeCold
	if resolved.Resume {
		resolvedMode = template.BootModeResume
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
		MemoryPath:   layout.SnapshotMemFile(),
		KernelPath:   layout.KernelFile(),
		InitrdPath:   layout.InitrdFile(),
		SnapfilePath: layout.SnapDir(),
		PmemPaths:    layout.PmemFiles(),
	}
}

func BootSpecFromRuntime(runtime BootRuntime) BootSpec {
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
	return BootSpec{
		MemorySizeMB: memSize,
		MemoryPath:   memoryPath,
		KernelPath:   filepath.Join(runtime.VMMount, common.VmKernelRelativePath),
		InitrdPath:   filepath.Join(runtime.VMMount, common.VmInitrdRelativePath),
		SnapfilePath: snapfilePath,
		PreGateKey:   runtime.BootIndexDigest,
	}
}
