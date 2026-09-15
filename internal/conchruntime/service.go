package conchruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/envd"
	"github.com/openeuler/Conch/internal/id"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandboxproxy"
	conchtemplate "github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/internal/volume"
	"github.com/openeuler/Conch/pkg/ulog"
)

type SandboxOps interface {
	Create(context.Context, sandbox.CreateRequest) (runtimeapi.SandboxCreateResult, error)
	Delete(context.Context, string) error
	Suspend(context.Context, string) error
	Resume(context.Context, string) error
	UpdateNetwork(context.Context, sandbox.NetworkUpdateRequest) error
	Checkpoint(context.Context, string, func(context.Context, sandbox.CheckpointResult) error) (sandbox.CheckpointResult, error)
}

type SnapshotOps interface {
	List(context.Context, runtimeapi.ListSnapshotsOptions) ([]runtimeapi.SnapshotRecord, error)
	Remove(context.Context, runtimeapi.RemoveSnapshotOptions) error
	Info(context.Context, runtimeapi.SnapshotInfoOptions) (runtimeapi.SnapshotRecord, error)
}

// Service adapts API requests and resolves Template names. Sandbox lifecycle
// state, leases and resource ownership belong to sandbox.Manager.
type Service struct {
	Sandbox         SandboxOps
	Containerd      *containerdclient.Client
	Snapshot        SnapshotOps
	Store           sandbox.Store
	Templates       conchtemplate.Store
	SandboxDefaults SandboxDefaults
	lifecycleLocks  sandboxLifecycleLocks
	// Capacity bounds concurrent sandbox CPU/memory reservations when set.
	Capacity *Capacity
	Envd     *envd.Client
	// ProxyRoutes routes E2B data-plane traffic to this Node's guests.
	ProxyRoutes     *sandboxproxy.Registry
	createSuccesses atomic.Uint64
	createFailures  atomic.Uint64
	rosterMu        sync.Mutex
}

func New(sandboxOps SandboxOps, client *containerdclient.Client) *Service {
	return &Service{Sandbox: sandboxOps, Containerd: client}
}

func (s *Service) SetSandboxDefaults(defaults SandboxDefaults) {
	if s == nil {
		return
	}
	s.SandboxDefaults = defaults
}

func (s *Service) CreateSandbox(ctx context.Context, opts SandboxCreateOptions) (result SandboxCreateResult, err error) {
	if s == nil || s.Sandbox == nil {
		return SandboxCreateResult{}, fmt.Errorf("sandbox service is not configured")
	}
	defer func() {
		if err != nil {
			s.createFailures.Add(1)
		} else {
			s.createSuccesses.Add(1)
		}
	}()
	if opts.E2B && (s.Envd == nil || s.ProxyRoutes == nil || s.Store == nil) {
		return SandboxCreateResult{}, fmt.Errorf("E2B runtime is not configured")
	}
	opts.SandboxID = strings.TrimSpace(opts.SandboxID)
	if opts.SandboxID == "" {
		generated, err := id.New()
		if err != nil {
			return SandboxCreateResult{}, err
		}
		opts.SandboxID = generated
	} else if err := id.Validate(opts.SandboxID); err != nil {
		return SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(
			fmt.Errorf("invalid sandbox_id: %w", err),
		)
	}
	unlock := s.lifecycleLocks.lock(opts.SandboxID)
	defer unlock()
	if s.Store != nil {
		if _, err := s.Store.Get(ctx, opts.SandboxID); err == nil {
			return SandboxCreateResult{}, sandbox.ErrAlreadyExists.Wrap(fmt.Errorf("sandbox %s already exists", opts.SandboxID))
		} else if !errors.Is(err, sandbox.ErrNotFound) {
			return SandboxCreateResult{}, fmt.Errorf("get sandbox state: %w", err)
		}
	}
	s.applySandboxDefaults(&opts)
	selection, err := s.resolveSandboxTemplate(ctx, opts.TemplateName, opts.TemplateID)
	if err != nil {
		return SandboxCreateResult{}, err
	}
	// Resume sandboxes boot with the resources captured at checkpoint time;
	// they win over defaults and caller values so the restored record tracks
	// the physical memory file.
	if selection.CPUCount > 0 {
		opts.VCPUNum = selection.CPUCount
		if opts.VCPUMax < opts.VCPUNum {
			opts.VCPUMax = opts.VCPUNum
		}
	}
	if selection.MemorySizeMB > 0 {
		opts.RamMB = selection.MemorySizeMB
	}
	if opts.VCPUNum < 1 || opts.VCPUMax < opts.VCPUNum {
		return SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("invalid sandbox CPU configuration"))
	}
	if opts.RamMB < 1 {
		return SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("ram_mb must be positive"))
	}
	if err := s.validateSandboxLimits(opts); err != nil {
		return SandboxCreateResult{}, err
	}
	capacityReserved := opts.E2B
	if capacityReserved {
		if err := s.Capacity.reserve(opts.SandboxID, opts.VCPUNum, opts.RamMB); err != nil {
			return SandboxCreateResult{}, err
		}
	}
	keepReservation := false
	defer func() {
		if capacityReserved && !keepReservation {
			s.Capacity.release(opts.SandboxID)
		}
	}()
	runtimeID, err := id.New()
	if err != nil {
		return SandboxCreateResult{}, err
	}
	var volumeMounts []volume.Mount
	var volumeMountRecords []sandbox.VolumeMountRecord
	for _, spec := range opts.VolumeMounts {
		volumeMounts = append(volumeMounts, volume.Mount{Source: spec.Source, Path: spec.Path})
		volumeMountRecords = append(volumeMountRecords, sandbox.VolumeMountRecord{Name: spec.Name, Path: spec.Path})
	}
	var routeGeneration uint64
	if opts.E2B {
		routeGeneration = s.ProxyRoutes.Begin(opts.SandboxID)
		defer func() {
			if err != nil {
				s.ProxyRoutes.Remove(opts.SandboxID, routeGeneration)
			}
		}()
	}
	// Manager owns the CREATING/READY record lifecycle, the create lease and
	// the agent token. On failure it retains the record when its cleanup
	// cannot confirm resource release and deletes it otherwise; a retained
	// record still owns this reservation.
	createResult, err := s.Sandbox.Create(ctx, sandbox.CreateRequest{
		RuntimeID:  runtimeID,
		TemplateID: selection.ID, TemplateName: selection.Name,
		SandboxID: opts.SandboxID, VMMName: opts.VMMName,
		VCPUNum: opts.VCPUNum, VCPUMax: opts.VCPUMax, RAMMB: opts.RamMB,
		Env: copyMap(opts.Env), VolumeMounts: volumeMounts, Network: opts.Network,
	})
	if err != nil {
		if capacityReserved && s.Store != nil {
			if _, getErr := s.Store.Get(ctx, opts.SandboxID); getErr == nil || !errors.Is(getErr, sandbox.ErrNotFound) {
				keepReservation = true
			}
		}
		return SandboxCreateResult{}, err
	}
	// rollbackAfterCreate tears down a created sandbox whose E2B bootstrap or
	// state persistence failed. Manager.Delete retains the record when its own
	// cleanup cannot confirm release; the reservation follows that owner.
	rollbackAfterCreate := func(cause error) error {
		cleanupErr := s.Sandbox.Delete(ctx, opts.SandboxID)
		if errors.Is(cleanupErr, sandbox.ErrNotFound) {
			cleanupErr = nil
		}
		if cleanupErr != nil {
			keepReservation = true
		}
		return combineOperationErrors(cause, cleanupErr)
	}
	envdVersion := ""
	if opts.E2B {
		generationCtx, current := s.ProxyRoutes.GenerationContext(opts.SandboxID, routeGeneration)
		if !current {
			generationCtx = ctx
		}
		// Keep the remaining creation deadline and synchronous generation
		// cancellation; envd initialization has no separate timeout budget.
		var initCtx context.Context
		var cancel context.CancelFunc
		if deadline, ok := ctx.Deadline(); ok {
			initCtx, cancel = context.WithDeadline(generationCtx, deadline)
		} else {
			initCtx, cancel = context.WithCancel(generationCtx)
		}
		stop := context.AfterFunc(ctx, cancel)
		if !current || ctx.Err() != nil {
			cancel()
		}
		err = s.Envd.WaitReady(initCtx, createResult.IP)
		if err == nil {
			err = s.Envd.Init(initCtx, createResult.IP, envd.InitOptions{
				EnvVars: copyMap(opts.Env), DefaultUser: "user", DefaultWorkdir: "/home/user",
			})
		}
		if err == nil {
			envdVersion, err = s.Envd.Version(initCtx, createResult.IP, createResult.AgentToken)
		}
		cancel()
		stop()
		if err != nil {
			return SandboxCreateResult{}, rollbackAfterCreate(fmt.Errorf("initialize envd: %w", err))
		}
		// Manager persisted the READY record; overlay the E2B fields it does
		// not know about before publishing the data-plane route.
		rec, getErr := s.Store.Get(ctx, opts.SandboxID)
		if getErr != nil {
			return SandboxCreateResult{}, rollbackAfterCreate(fmt.Errorf("read created sandbox state: %w", getErr))
		}
		rec.E2B = true
		rec.EnvdVersion = envdVersion
		rec.Metadata = copyMap(opts.Metadata)
		rec.TimeoutAction = opts.TimeoutAction
		rec.MaskRequestHost = opts.MaskRequestHost
		rec.VolumeMounts = volumeMountRecords
		// The upstream treats a zero TTL as an immediately due deadline.
		rec.ExpiresAt = time.Now().Add(opts.Timeout).UnixNano()
		unlockRoster := s.LockSandboxRoster()
		_, err = s.Store.Update(ctx, rec)
		unlockRoster()
		if err != nil {
			return SandboxCreateResult{}, rollbackAfterCreate(fmt.Errorf("persist sandbox state: %w", err))
		}
		if err := s.ProxyRoutes.Publish(opts.SandboxID, routeGeneration, createResult.IP); err != nil {
			return SandboxCreateResult{}, rollbackAfterCreate(err)
		}
	}
	keepReservation = capacityReserved
	return SandboxCreateResult{
		SandboxID:    opts.SandboxID,
		IP:           createResult.IP,
		AgentToken:   createResult.AgentToken,
		TemplateName: selection.Name,
		TemplateID:   selection.ID,
		VCPUNum:      opts.VCPUNum,
		RamMB:        opts.RamMB,
		CreatedAt:    createResult.CreatedAt,
		EnvdVersion:  envdVersion,
	}, nil
}

func (s *Service) validateSandboxLimits(opts SandboxCreateOptions) error {
	if opts.VCPUNum > runtimeapi.SandboxMaxVCPU || opts.VCPUMax > runtimeapi.SandboxMaxVCPU {
		return sandbox.ErrResourceExhausted.Wrap(fmt.Errorf(
			"requested vcpu_num=%d and vcpu_max=%d exceed maximum %d",
			opts.VCPUNum, opts.VCPUMax, runtimeapi.SandboxMaxVCPU,
		))
	}
	if opts.RamMB > runtimeapi.SandboxMaxRAMMB {
		return sandbox.ErrResourceExhausted.Wrap(fmt.Errorf(
			"requested ram_mb=%d exceeds maximum %d",
			opts.RamMB, runtimeapi.SandboxMaxRAMMB,
		))
	}
	return nil
}

func (s *Service) SuspendSandbox(ctx context.Context, sandboxID string) error {
	return s.Sandbox.Suspend(ctx, sandboxID)
}

func (s *Service) ResumeSandbox(ctx context.Context, sandboxID string) error {
	return s.Sandbox.Resume(ctx, sandboxID)
}

func (s *Service) UpdateSandboxNetworkConfig(ctx context.Context, opts SandboxNetworkUpdateOptions) error {
	return s.Sandbox.UpdateNetwork(ctx, sandbox.NetworkUpdateRequest{SandboxID: opts.SandboxID, Network: opts.Network})
}

// Template registration stays at the API boundary. Manager calls it under the
// checkpoint lease and lifecycle lock, and owns head persistence and rollback.
func (s *Service) CheckpointSandbox(ctx context.Context, opts SandboxCheckpointOptions) (SandboxCheckpointResult, error) {
	if s == nil || s.Sandbox == nil {
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox service is not configured")
	}
	if s.Templates == nil {
		return SandboxCheckpointResult{}, fmt.Errorf("template store is not configured")
	}
	name := strings.TrimSpace(opts.TemplateName)
	if name == "" {
		return SandboxCheckpointResult{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("template_name is required"))
	}
	result, err := s.Sandbox.Checkpoint(ctx, opts.SandboxID, func(ctx context.Context, result sandbox.CheckpointResult) error {
		_, err := s.Templates.Put(ctx, conchtemplate.Entry{
			Name: name, Origin: conchtemplate.OriginCheckpoint, BootMode: conchtemplate.BootModeResume,
			BootIndexDigest: result.BootIndexDigest, ParentBootIndexDigest: result.ParentBootIndexDigest,
			SourceSandboxID: strings.TrimSpace(opts.SandboxID), Labels: copyMap(opts.Labels),
		}, result.Target)
		return err
	})
	if err != nil {
		return SandboxCheckpointResult{}, err
	}
	return SandboxCheckpointResult{TemplateID: result.BootIndexDigest}, nil
}

func (s *Service) applySandboxDefaults(opts *SandboxCreateOptions) {
	if s == nil || opts == nil {
		return
	}
	defaults := s.SandboxDefaults
	opts.TemplateName = strings.TrimSpace(opts.TemplateName)
	opts.TemplateID = strings.TrimSpace(opts.TemplateID)
	if opts.TemplateName == "" && opts.TemplateID == "" {
		opts.TemplateName = strings.TrimSpace(defaults.TemplateName)
		opts.TemplateID = strings.TrimSpace(defaults.TemplateID)
	}
	if opts.VMMName == "" {
		opts.VMMName = defaults.VMMName
	}
	if opts.VCPUNum == 0 {
		opts.VCPUNum = defaults.VCPUNum
	}
	if opts.VCPUMax == 0 {
		opts.VCPUMax = defaults.VCPUMax
	}
	if opts.RamMB == 0 {
		opts.RamMB = defaults.RamMB
	}
}

type sandboxTemplateSelection struct {
	Name         string
	ID           string
	MemorySizeMB int64
	CPUCount     int64
	Resume       bool
}

func (s *Service) resolveSandboxTemplate(ctx context.Context, name, rawID string) (sandboxTemplateSelection, error) {
	name = strings.TrimSpace(name)
	rawID = strings.TrimSpace(rawID)
	if (name == "") == (rawID == "") {
		return sandboxTemplateSelection{}, sandbox.ErrInvalidArgument.Wrap(
			fmt.Errorf("exactly one of template_name or template_id is required"),
		)
	}
	if name != "" {
		if s.Templates == nil {
			return sandboxTemplateSelection{}, fmt.Errorf("template store is not configured")
		}
		entry, err := s.Templates.Get(ctx, name)
		if err != nil {
			return sandboxTemplateSelection{}, err
		}
		selection := sandboxTemplateSelection{Name: entry.Name, ID: entry.BootIndexDigest}
		if entry.BootMode == conchtemplate.BootModeResume {
			info, err := conchimage.InspectBootIndex(ctx, s.Containerd, entry.BootIndexDigest)
			if err != nil {
				return sandboxTemplateSelection{}, err
			}
			selection.MemorySizeMB = info.MemorySizeMB
			selection.CPUCount = info.CPUCount
			selection.Resume = true
		}
		return selection, nil
	}
	parsedID, err := digest.Parse(rawID)
	if err != nil {
		return sandboxTemplateSelection{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("invalid template_id %q: %w", rawID, err))
	}
	return sandboxTemplateSelection{ID: parsedID.String()}, nil
}

// PullTemplate fetches and statically validates a registry Boot Index before
// creating the local Template entry. Runtime boot validation belongs to
// integration tests, not the pull request path.
func (s *Service) PullTemplate(ctx context.Context, opts TemplatePullOptions) (TemplatePullResult, error) {
	if s == nil || s.Containerd == nil {
		return TemplatePullResult{}, fmt.Errorf("containerd client is required")
	}
	if s.Templates == nil {
		return TemplatePullResult{}, fmt.Errorf("template store is not configured")
	}
	reference := strings.TrimSpace(opts.Reference)
	if reference == "" {
		return TemplatePullResult{}, conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("template reference is required"))
	}
	var entry conchtemplate.Entry
	consumed := false
	err := conchimage.WithPulledBootIndex(ctx, s.Containerd, conchimage.RegistryPullOptions{
		Reference: reference,
		PlainHTTP: opts.PlainHTTP,
		Username:  opts.Username,
		Password:  opts.Password,
	}, func(pullCtx context.Context, pulled conchimage.PulledBootIndex) error {
		consumed = true
		info := pulled.Info
		origin := conchtemplate.OriginImage
		bootMode := conchtemplate.BootModeCold
		if info.Resume {
			origin = conchtemplate.OriginCheckpoint
			bootMode = conchtemplate.BootModeResume
		}
		var err error
		entry, err = s.Templates.Put(pullCtx, conchtemplate.Entry{
			Name:            pulled.SourceImageName,
			Origin:          origin,
			BootMode:        bootMode,
			BootIndexDigest: info.BootIndexDigest,
			SourceRef:       reference,
			Labels:          opts.Labels,
		}, pulled.Target)
		return err
	})
	if err != nil {
		if consumed {
			return TemplatePullResult{}, err
		}
		return TemplatePullResult{}, fmt.Errorf("pull template boot index %s: %w", reference, translateTemplateArtifactError(err))
	}
	return TemplatePullResult{
		Name:       entry.Name,
		TemplateID: entry.BootIndexDigest,
	}, nil
}

// PushTemplate publishes the descriptor closure rooted at the Template's
// immutable BootIndexDigest.
func (s *Service) PushTemplate(ctx context.Context, opts TemplatePushOptions) error {
	if s == nil || s.Containerd == nil {
		return fmt.Errorf("containerd client is required")
	}
	if s.Templates == nil {
		return fmt.Errorf("template store is not configured")
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("template name is required"))
	}
	remoteReference := strings.TrimSpace(opts.RemoteReference)
	if remoteReference == "" {
		return conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("remote template reference is required"))
	}
	rec, err := s.Templates.Get(ctx, name)
	if err != nil {
		return err
	}
	bootIndexDigest := strings.TrimSpace(rec.BootIndexDigest)
	if bootIndexDigest == "" {
		return conchtemplate.ErrFailedPrecondition.Wrap(fmt.Errorf("template has no boot index digest"))
	}
	return conchimage.PushBootIndex(ctx, s.Containerd, conchimage.PushBootIndexOptions{
		BootIndexDigest: bootIndexDigest,
		RemoteReference: remoteReference,
		PlainHTTP:       opts.PlainHTTP,
		Username:        opts.Username,
		Password:        opts.Password,
	})
}

func (s *Service) UnpackTemplate(ctx context.Context, opts TemplateUnpackOptions) error {
	if s == nil || s.Containerd == nil {
		return fmt.Errorf("containerd client is required")
	}
	if s.Templates == nil {
		return fmt.Errorf("template store is not configured")
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("template name is required"))
	}
	rec, err := s.Templates.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("get template %s: %w", name, err)
	}
	if err := conchimage.UnpackBootIndex(ctx, s.Containerd, rec.BootIndexDigest); err != nil {
		return fmt.Errorf("unpack template %s: %w", name, translateTemplateArtifactError(err))
	}
	return nil
}

func (s *Service) CreateTemplate(ctx context.Context, opts TemplateCreateOptions) (TemplateCreateResult, error) {
	if s == nil || s.Containerd == nil {
		return TemplateCreateResult{}, fmt.Errorf("containerd client is required")
	}
	if s.Templates == nil {
		return TemplateCreateResult{}, fmt.Errorf("template store is not configured")
	}
	opts.Name = strings.TrimSpace(opts.Name)
	if opts.Name == "" {
		return TemplateCreateResult{}, conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("template name is required"))
	}
	source := strings.TrimSpace(opts.Source)
	if source == "" {
		return TemplateCreateResult{}, conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("template source is required"))
	}
	if strings.TrimSpace(opts.KernelPath) == "" || strings.TrimSpace(opts.InitrdPath) == "" {
		return TemplateCreateResult{}, conchtemplate.ErrInvalidArtifact.Wrap(fmt.Errorf("kernel and initrd are required"))
	}
	opts.Source = source
	result, err := s.createTemplateFromSource(ctx, opts)
	if err != nil {
		return TemplateCreateResult{}, err
	}
	return TemplateCreateResult{
		Name:       result.entry.Name,
		TemplateID: result.entry.BootIndexDigest,
	}, nil
}

type templateBuildResult struct {
	entry conchtemplate.Entry
}

func (s *Service) createTemplateFromSource(ctx context.Context, opts TemplateCreateOptions) (templateBuildResult, error) {
	sourceCtx, err := s.Containerd.WithNamespace(ctx)
	if err != nil {
		return templateBuildResult{}, fmt.Errorf("prepare rootfs source namespace: %w", err)
	}
	sourceImage, err := s.Containerd.GetImage(sourceCtx, opts.Source)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return templateBuildResult{}, fmt.Errorf("lookup rootfs source image %s: %w", opts.Source, err)
		}
		if err := conchimage.Pull(ctx, s.Containerd, runtimeapi.PullImageOptions{
			ImageName: opts.Source,
			PlainHTTP: opts.PlainHTTP,
			Username:  opts.Username,
			Password:  opts.Password,
		}); err != nil {
			return templateBuildResult{}, fmt.Errorf("pull rootfs source image %s: %w", opts.Source, err)
		}
		sourceImage, err = s.Containerd.GetImage(sourceCtx, opts.Source)
		if err != nil {
			return templateBuildResult{}, fmt.Errorf("resolve pulled rootfs source image %s: %w", opts.Source, err)
		}
	}
	sourceKind, err := conchimage.DetectImageKind(sourceCtx, s.Containerd.ContentStore(), sourceImage.Target())
	if err != nil {
		return templateBuildResult{}, fmt.Errorf("classify rootfs source image %s: %w", sourceImage.Name(), err)
	}
	if sourceKind != conchimage.ImageKindOCIImage {
		return templateBuildResult{}, conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf(
			"Template image %s cannot be used as a rootfs source", sourceImage.Name(),
		))
	}

	buildID, err := id.New()
	if err != nil {
		return templateBuildResult{}, err
	}
	convertTarget := fmt.Sprintf("conch-erofs-rootfs:%s", buildID)
	converted, err := erofsconvert.ConvertRootfs(ctx, s.Containerd, erofsconvert.ConvertRootfsRequest{
		SourceImage: sourceImage.Name(),
		TargetImage: convertTarget,
		MkfsOptions: []string{erofsconvert.DefaultMkfsOption},
		AlignBytes:  erofsconvert.DefaultAlignBytes,
	})
	if err != nil {
		return templateBuildResult{}, conchimage.ErrConversionFailed.Wrap(fmt.Errorf("convert rootfs to EROFS: %w", err))
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := conchimage.Remove(cleanupCtx, s.Containerd, runtimeapi.RemoveImageOptions{
			ImageName: converted.ImageName,
		}); err != nil {
			ulog.GetLogger().Warn("failed to remove temporary converted rootfs image",
				ulog.F("image", converted.ImageName),
				ulog.F("error", err))
		}
	}()

	publishCtx, done, err := s.Containerd.WithLease(sourceCtx)
	if err != nil {
		return templateBuildResult{}, fmt.Errorf("create Template content lease: %w", err)
	}
	defer done(publishCtx)
	published, err := conchimage.PublishBootIndex(publishCtx, s.Containerd, conchimage.PublishBootIndexOptions{
		RootfsImageName: converted.ImageName,
		KernelPath:      opts.KernelPath,
		InitrdPath:      opts.InitrdPath,
	})
	if err != nil {
		return templateBuildResult{}, fmt.Errorf("publish boot image: %w", err)
	}
	entry, err := s.Templates.Put(publishCtx, conchtemplate.Entry{
		Name:            opts.Name,
		Origin:          conchtemplate.OriginImage,
		BootMode:        conchtemplate.BootModeCold,
		BootIndexDigest: published.BootIndexDigest,
		SourceRef:       opts.Source,
		Labels:          opts.Labels,
	}, published.Target)
	if err != nil {
		return templateBuildResult{}, err
	}

	return templateBuildResult{
		entry: entry,
	}, nil
}

func (s *Service) ListTemplates(ctx context.Context, opts runtimeapi.TemplateListOptions) ([]runtimeapi.TemplateRecord, error) {
	if s == nil || s.Templates == nil {
		return nil, fmt.Errorf("template store is not configured")
	}
	items, err := s.Templates.List(ctx, conchtemplate.Filter{
		Origin:   conchtemplate.Origin(strings.TrimSpace(opts.Origin)),
		BootMode: conchtemplate.BootMode(strings.TrimSpace(opts.BootMode)),
	})
	if err != nil {
		return nil, err
	}
	out := make([]runtimeapi.TemplateRecord, 0, len(items))
	for _, item := range items {
		out = append(out, publicTemplateRecord(item))
	}
	return out, nil
}

func (s *Service) GetTemplate(ctx context.Context, name string) (runtimeapi.TemplateRecord, error) {
	if s == nil || s.Templates == nil {
		return runtimeapi.TemplateRecord{}, fmt.Errorf("template store is not configured")
	}
	rec, err := s.Templates.Get(ctx, name)
	if err != nil {
		return runtimeapi.TemplateRecord{}, err
	}
	return publicTemplateRecord(rec), nil
}

func (s *Service) RemoveTemplate(ctx context.Context, name string) error {
	if s == nil || s.Templates == nil {
		return fmt.Errorf("template store is not configured")
	}
	return s.Templates.Delete(ctx, name)
}

func publicTemplateRecord(entry conchtemplate.Entry) runtimeapi.TemplateRecord {
	return runtimeapi.TemplateRecord{
		Name:             entry.Name,
		TemplateID:       entry.BootIndexDigest,
		Origin:           string(entry.Origin),
		BootMode:         string(entry.BootMode),
		ParentTemplateID: entry.ParentBootIndexDigest,
		SourceSandboxID:  entry.SourceSandboxID,
		SourceRef:        entry.SourceRef,
		Labels:           copyMap(entry.Labels),
		CreatedAt:        entry.CreatedAt,
	}
}

func (s *Service) ListSnapshots(ctx context.Context, opts runtimeapi.ListSnapshotsOptions) ([]runtimeapi.SnapshotRecord, error) {
	if s == nil || s.Snapshot == nil {
		return nil, fmt.Errorf("snapshot service is not configured")
	}
	return s.Snapshot.List(ctx, opts)
}

func (s *Service) RemoveSnapshot(ctx context.Context, opts runtimeapi.RemoveSnapshotOptions) error {
	if s == nil || s.Snapshot == nil {
		return fmt.Errorf("snapshot service is not configured")
	}
	return s.Snapshot.Remove(ctx, opts)
}

func (s *Service) SnapshotInfo(ctx context.Context, opts runtimeapi.SnapshotInfoOptions) (runtimeapi.SnapshotRecord, error) {
	if s == nil || s.Snapshot == nil {
		return runtimeapi.SnapshotRecord{}, fmt.Errorf("snapshot service is not configured")
	}
	return s.Snapshot.Info(ctx, opts)
}

func translateTemplateArtifactError(err error) error {
	if errors.Is(err, conchimage.ErrInvalidArgument) || errors.Is(err, conchimage.ErrInvalidContent) {
		return conchtemplate.ErrInvalidArtifact.Wrap(err)
	}
	return err
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
