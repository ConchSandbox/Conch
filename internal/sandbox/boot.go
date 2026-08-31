package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/vmm"
)

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
	snapshots      SnapshotBackend
	resolveContext context.Context
	resolveTimeout time.Duration
	resolveBoot    func(context.Context, string) (conchimage.ResolvedBoot, error)
	resolveGroup   singleflight.Group
}

func NewBootPreparer(ctx context.Context, snapshots SnapshotBackend, client *containerdclient.Client, timeout time.Duration) (BootPreparer, error) {
	if client == nil || client.Client == nil {
		return nil, fmt.Errorf("containerd client is required")
	}
	return newBootPreparer(ctx, snapshots, timeout, func(ctx context.Context, bootIndexDigest string) (conchimage.ResolvedBoot, error) {
		return conchimage.ResolveBoot(ctx, client, bootIndexDigest)
	})
}

func newBootPreparer(
	ctx context.Context,
	snapshots SnapshotBackend,
	timeout time.Duration,
	resolveBoot func(context.Context, string) (conchimage.ResolvedBoot, error),
) (BootPreparer, error) {
	if ctx == nil {
		return nil, fmt.Errorf("boot resolver context is required")
	}
	if snapshots == nil {
		return nil, fmt.Errorf("snapshot backend is required")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("boot resolver timeout must be positive")
	}
	if resolveBoot == nil {
		return nil, fmt.Errorf("boot resolver is required")
	}
	return &bootPreparer{
		snapshots:      snapshots,
		resolveContext: ctx,
		resolveTimeout: timeout,
		resolveBoot:    resolveBoot,
	}, nil
}

func (p *bootPreparer) Prepare(ctx context.Context, req PrepareBootRequest) (PreparedBoot, error) {
	if p == nil || p.snapshots == nil || p.resolveContext == nil || p.resolveTimeout <= 0 || p.resolveBoot == nil {
		return PreparedBoot{}, fmt.Errorf("sandbox boot preparer is not configured")
	}
	key := strings.TrimSpace(req.SandboxID)
	if key == "" {
		return PreparedBoot{}, fmt.Errorf("sandbox_id is required")
	}
	resolved, err := p.resolveTemplate(ctx, req.TemplateID)
	if err != nil {
		return PreparedBoot{}, err
	}
	if err := validateResolvedBoot(resolved, strings.TrimSpace(req.VMMName)); err != nil {
		return PreparedBoot{}, fmt.Errorf("template %s: %w", resolved.BootIndexDigest, err)
	}
	return p.prepareResolvedBoot(ctx, key, req.VMMName, req.RAMMB, resolved)
}

func (p *bootPreparer) resolveTemplate(
	ctx context.Context,
	id string,
) (conchimage.ResolvedBoot, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return conchimage.ResolvedBoot{}, fmt.Errorf("template_id is required")
	}
	if err := ctx.Err(); err != nil {
		return conchimage.ResolvedBoot{}, fmt.Errorf("resolve template boot index %s: %w", id, err)
	}
	result := p.resolveGroup.DoChan(id, func() (value any, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("boot resolver panicked: %v", recovered)
			}
		}()
		resolveCtx, cancel := context.WithTimeout(p.resolveContext, p.resolveTimeout)
		defer cancel()
		return p.resolveBoot(resolveCtx, id)
	})
	select {
	case <-ctx.Done():
		return conchimage.ResolvedBoot{}, fmt.Errorf("resolve template boot index %s: %w", id, ctx.Err())
	case call := <-result:
		if err := ctx.Err(); err != nil {
			return conchimage.ResolvedBoot{}, fmt.Errorf("resolve template boot index %s: %w", id, err)
		}
		if call.Err != nil {
			return conchimage.ResolvedBoot{}, fmt.Errorf("resolve template boot index %s: %w", id, call.Err)
		}
		resolved := call.Val.(conchimage.ResolvedBoot)
		if resolved.BootIndexDigest != id {
			return conchimage.ResolvedBoot{}, fmt.Errorf(
				"resolved boot index digest %s does not match template digest %s",
				resolved.BootIndexDigest,
				id,
			)
		}
		return resolved, nil
	}
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
	return PreparedBoot{
		Spec: bootSpecFromLayout(layout),
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

func validateResolvedBoot(resolved conchimage.ResolvedBoot, requestedVMM string) error {
	if strings.TrimSpace(resolved.RootfsKey) == "" || strings.TrimSpace(resolved.VMKey) == "" {
		return fmt.Errorf("boot index unpack returned incomplete parents")
	}
	if resolved.Resume {
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
	}
}
