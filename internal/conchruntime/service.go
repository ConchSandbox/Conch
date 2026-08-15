package conchruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/internal/apperror"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandboxid"
	conchtemplate "github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/pkg/ulog"
)

type SandboxOps interface {
	Create(sandbox.CreateRequest) (sandbox.CreateResult, error)
	Delete(sandbox.DeleteRequest) error
	Suspend(sandbox.LifecycleRequest) error
	Resume(sandbox.LifecycleRequest) error
	UpdateNetwork(context.Context, sandbox.NetworkUpdateRequest) error
	Checkpoint(sandbox.CheckpointRequest) (sandbox.CheckpointResult, error)
}

type SnapshotOps interface {
	List(context.Context, runtimeapi.ListSnapshotsOptions) ([]runtimeapi.SnapshotRecord, error)
	Remove(context.Context, runtimeapi.RemoveSnapshotOptions) error
	Info(context.Context, runtimeapi.SnapshotInfoOptions) (runtimeapi.SnapshotRecord, error)
}

type Service struct {
	Sandbox         SandboxOps
	Containerd      *containerdclient.Client
	Snapshot        SnapshotOps
	Store           state.Store
	Templates       conchtemplate.Store
	SandboxDefaults SandboxDefaults
	lifecycleLocks  sandboxLifecycleLocks
}

type sandboxLifecycleLock struct {
	mu   sync.Mutex
	refs int
}

type sandboxLifecycleLocks struct {
	mu      sync.Mutex
	entries map[string]*sandboxLifecycleLock
}

func (l *sandboxLifecycleLocks) lock(id string) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*sandboxLifecycleLock)
	}
	entry := l.entries[id]
	if entry == nil {
		entry = &sandboxLifecycleLock{}
		l.entries[id] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 && l.entries[id] == entry {
			delete(l.entries, id)
		}
		l.mu.Unlock()
	}
}

func New(sandboxOps SandboxOps, client *containerdclient.Client, store state.Store) *Service {
	return &Service{
		Sandbox:    sandboxOps,
		Containerd: client,
		Store:      store,
		Templates:  conchtemplate.NewStore(store),
	}
}

func (s *Service) SetSandboxDefaults(defaults SandboxDefaults) {
	if s == nil {
		return
	}
	s.SandboxDefaults = defaults
}

func (s *Service) CreateSandbox(ctx context.Context, opts SandboxCreateOptions) (SandboxCreateResult, error) {
	if s == nil || s.Sandbox == nil {
		return SandboxCreateResult{}, fmt.Errorf("sandbox service is not configured")
	}
	if err := agentprotocol.ValidateEnvironment(opts.Env); err != nil {
		return SandboxCreateResult{}, sandbox.ErrInvalidEnvironment.Wrap(err)
	}
	opts.SandboxID = strings.TrimSpace(opts.SandboxID)
	if opts.SandboxID == "" {
		id, err := NewID()
		if err != nil {
			return SandboxCreateResult{}, err
		}
		opts.SandboxID = id
	} else {
		if err := sandboxid.Validate(opts.SandboxID); err != nil {
			return SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(
				fmt.Errorf("invalid sandbox_id: %w", err),
			)
		}
	}
	unlock := s.lifecycleLocks.lock(opts.SandboxID)
	defer unlock()
	if s.Store != nil {
		if _, err := s.Store.GetSandbox(ctx, opts.SandboxID); err == nil {
			return SandboxCreateResult{}, sandbox.ErrAlreadyExists.Wrap(fmt.Errorf("sandbox %s already exists", opts.SandboxID))
		} else if !errors.Is(err, state.ErrNotFound) {
			return SandboxCreateResult{}, fmt.Errorf("get sandbox state: %w", err)
		}
	}
	if opts.LeaseID == "" {
		opts.LeaseID = containerdclient.RuntimeLeaseID()
	}
	s.applySandboxDefaults(&opts)
	if opts.TemplateID == "" {
		return SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("template_id is required and no default_spec.template_id is configured"))
	}
	parsedTemplateID, err := digest.Parse(opts.TemplateID)
	if err != nil {
		return SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("invalid template_id %q: %w", opts.TemplateID, err))
	}
	opts.TemplateID = parsedTemplateID.String()
	if opts.VCPUNum < 1 || opts.VCPUMax < opts.VCPUNum {
		return SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("invalid sandbox CPU configuration"))
	}
	if opts.RamMB < 1 {
		return SandboxCreateResult{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("ram_mb must be positive"))
	}
	if err := s.validateSandboxLimits(opts); err != nil {
		return SandboxCreateResult{}, err
	}
	if err := netstack.ValidateSandboxNetworkInputConfig(ctx, opts.Network); err != nil {
		return SandboxCreateResult{}, err
	}
	agentToken, err := sandbox.GenerateAgentToken()
	if err != nil {
		return SandboxCreateResult{}, err
	}

	req := sandbox.CreateRequest{
		TemplateID:   opts.TemplateID,
		VMMName:      opts.VMMName,
		SandboxID:    opts.SandboxID,
		LeaseID:      opts.LeaseID,
		VCPUNum:      opts.VCPUNum,
		VCPUMax:      opts.VCPUMax,
		RAMMB:        opts.RamMB,
		AgentToken:   agentToken,
		Env:          copyMap(opts.Env),
		VolumeMounts: opts.VolumeMounts,
		Network:      opts.Network,
	}

	createdAt := time.Now().UnixNano()
	creatingRecord := state.SandboxRecord{
		SandboxID:        opts.SandboxID,
		State:            state.SandboxCreating,
		CreatedAt:        createdAt,
		SourceTemplateID: opts.TemplateID,
		VCPUNum:          opts.VCPUNum,
		RamMB:            opts.RamMB,
		Network:          opts.Network,
	}
	if err := s.upsertSandbox(ctx, creatingRecord); err != nil {
		return SandboxCreateResult{}, fmt.Errorf("persist creating sandbox state: %w", err)
	}
	deleteCreatingRecord := func() error {
		if s.Store == nil {
			return nil
		}
		return s.Store.DeleteSandbox(context.Background(), opts.SandboxID)
	}

	createResult, err := s.Sandbox.Create(req)
	if err != nil {
		return SandboxCreateResult{}, combineOperationErrors(translateSandboxError(err), deleteCreatingRecord())
	}
	rec := state.SandboxRecord{
		SandboxID:                opts.SandboxID,
		VMMPID:                   createResult.VMMPID,
		State:                    state.SandboxReady,
		CreatedAt:                createdAt,
		SourceTemplateID:         opts.TemplateID,
		CheckpointHeadTemplateID: createResult.BootIndexDigest,
		IP:                       createResult.IP,
		VCPUNum:                  opts.VCPUNum,
		RamMB:                    opts.RamMB,
		Network:                  opts.Network,
	}
	if err := s.upsertSandbox(ctx, rec); err != nil {
		cleanupErr := s.Sandbox.Delete(sandbox.DeleteRequest{SandboxID: opts.SandboxID})
		deleteErr := deleteCreatingRecord()
		return SandboxCreateResult{}, combineOperationErrors(
			fmt.Errorf("persist sandbox state: %w", err),
			cleanupErr,
			deleteErr,
		)
	}
	return SandboxCreateResult{
		SandboxID:  opts.SandboxID,
		IP:         createResult.IP,
		AgentToken: createResult.AgentToken,
		TemplateID: opts.TemplateID,
		VCPUNum:    opts.VCPUNum,
		RamMB:      opts.RamMB,
		CreatedAt:  createdAt,
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

func (s *Service) UpdateSandboxNetworkConfig(ctx context.Context, opts SandboxNetworkUpdateOptions) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	if strings.TrimSpace(opts.SandboxID) == "" {
		return sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	if err := netstack.ValidateSandboxNetworkInputConfig(ctx, opts.Network); err != nil {
		return err
	}
	unlock := s.lifecycleLocks.lock(opts.SandboxID)
	defer unlock()
	rec, err := s.getSandbox(ctx, opts.SandboxID)
	if err != nil {
		return err
	}
	if rec.State != state.SandboxReady && rec.State != state.SandboxSuspended {
		return sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s is %s", opts.SandboxID, rec.State))
	}
	oldNetwork := rec.Network
	rec.Network = opts.Network
	rec.LastError = ""
	if err := s.upsertSandbox(ctx, rec); err != nil {
		return err
	}
	if err := s.Sandbox.UpdateNetwork(ctx, sandbox.NetworkUpdateRequest{SandboxID: opts.SandboxID, Network: opts.Network}); err != nil {
		rollbackCtx := context.WithoutCancel(ctx)
		rollbackErr := s.Sandbox.UpdateNetwork(rollbackCtx, sandbox.NetworkUpdateRequest{SandboxID: opts.SandboxID, Network: oldNetwork})
		rec.Network = oldNetwork
		applyErr := combineOperationErrors(err, rollbackErr)
		if rollbackErr != nil {
			rec.State = state.SandboxUnknown
			applyErr = combineOperationErrors(applyErr, s.Sandbox.Suspend(sandbox.LifecycleRequest{SandboxID: opts.SandboxID}))
		}
		rec.LastError = applyErr.Error()
		rollbackStoreErr := s.upsertSandbox(rollbackCtx, rec)
		return combineOperationErrors(applyErr, rollbackStoreErr)
	}
	return nil
}

func (s *Service) applySandboxDefaults(opts *SandboxCreateOptions) {
	if s == nil || opts == nil {
		return
	}
	defaults := s.SandboxDefaults
	opts.TemplateID = strings.TrimSpace(opts.TemplateID)
	if opts.TemplateID == "" {
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

func (s *Service) RemoveSandbox(ctx context.Context, sandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	defer unlock()
	err := s.Sandbox.Delete(sandbox.DeleteRequest{SandboxID: sandboxID})
	if err != nil && errors.Is(err, sandbox.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return err
	}
	if s.Store != nil {
		return s.Store.DeleteSandbox(ctx, sandboxID)
	}
	return nil
}

func (s *Service) SuspendSandbox(ctx context.Context, sandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, _ := s.getSandbox(ctx, sandboxID)
	err := s.Sandbox.Suspend(sandbox.LifecycleRequest{SandboxID: sandboxID})
	if rec.SandboxID != "" {
		rec.State = state.SandboxSuspended
		if err != nil {
			rec.State = state.SandboxUnknown
			rec.LastError = err.Error()
		} else {
			rec.LastError = ""
		}
		_ = s.upsertSandbox(ctx, rec)
	}
	return err
}

func (s *Service) ResumeSandbox(ctx context.Context, sandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	unlock := s.lifecycleLocks.lock(sandboxID)
	defer unlock()
	rec, _ := s.getSandbox(ctx, sandboxID)
	err := s.Sandbox.Resume(sandbox.LifecycleRequest{SandboxID: sandboxID})
	if rec.SandboxID != "" {
		rec.State = state.SandboxReady
		if err != nil {
			rec.State = state.SandboxUnknown
			rec.LastError = err.Error()
		} else {
			rec.LastError = ""
		}
		_ = s.upsertSandbox(ctx, rec)
	}
	return err
}

func (s *Service) CheckpointSandbox(ctx context.Context, opts SandboxCheckpointOptions) (SandboxCheckpointResult, error) {
	if s == nil || s.Sandbox == nil {
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox service is not configured")
	}
	opts.SandboxID = strings.TrimSpace(opts.SandboxID)
	if opts.SandboxID == "" {
		return SandboxCheckpointResult{}, sandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	unlock := s.lifecycleLocks.lock(opts.SandboxID)
	defer unlock()
	rec, err := s.getSandbox(ctx, opts.SandboxID)
	if err != nil {
		return SandboxCheckpointResult{}, err
	}
	if s.Containerd == nil {
		return SandboxCheckpointResult{}, fmt.Errorf("containerd client is not configured")
	}
	if s.Store == nil {
		return SandboxCheckpointResult{}, fmt.Errorf("checkpoint publisher is not configured")
	}
	sandboxID := rec.SandboxID
	parentID := strings.TrimSpace(rec.CheckpointHeadTemplateID)
	if parentID == "" {
		return SandboxCheckpointResult{}, sandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox %s has no checkpoint head Template ID", sandboxID))
	}

	captured, err := s.Sandbox.Checkpoint(sandbox.CheckpointRequest{
		SandboxID: sandboxID,
	})
	if err != nil {
		return SandboxCheckpointResult{}, err
	}
	defer os.RemoveAll(captured.MemRootPath)

	published, err := conchimage.PublishCheckpointBootIndex(ctx, s.Containerd, conchimage.PublishCheckpointBootIndexOptions{
		SourceBootIndexDigest: parentID,
		MemRoot:               captured.MemRootPath,
		VMMName:               captured.VMMName,
		MemorySizeMB:          captured.MemorySizeMB,
	})
	if err != nil {
		return SandboxCheckpointResult{}, err
	}
	keepCanonicalRecord := false
	defer func() {
		if !keepCanonicalRecord {
			s.cleanupCanonicalTemplateRecord(ctx, published.BootIndexDigest)
		}
	}()
	info, err := conchimage.InspectBootIndex(ctx, s.Containerd, published.BootIndexDigest)
	if err != nil {
		return SandboxCheckpointResult{}, fmt.Errorf("validate published checkpoint boot index: %w", err)
	}
	if !info.Resume {
		return SandboxCheckpointResult{}, fmt.Errorf("published checkpoint boot index is not resume-capable")
	}
	if info.BootIndexDigest != published.BootIndexDigest {
		return SandboxCheckpointResult{}, fmt.Errorf(
			"validated checkpoint boot index digest %s does not match published digest %s",
			info.BootIndexDigest,
			published.BootIndexDigest,
		)
	}
	if info.VMMName != captured.VMMName {
		return SandboxCheckpointResult{}, fmt.Errorf(
			"validated checkpoint VMM %s does not match captured VMM %s",
			info.VMMName,
			captured.VMMName,
		)
	}
	if info.MemorySizeMB != captured.MemorySizeMB {
		return SandboxCheckpointResult{}, fmt.Errorf(
			"validated checkpoint memory size %d MB does not match captured size %d MB",
			info.MemorySizeMB,
			captured.MemorySizeMB,
		)
	}
	if err := s.Store.PublishCheckpoint(ctx, conchtemplate.Entry{
		Origin:                conchtemplate.OriginCheckpoint,
		BootMode:              conchtemplate.BootModeResume,
		BootIndexDigest:       info.BootIndexDigest,
		ParentBootIndexDigest: parentID,
		SourceSandboxID:       sandboxID,
		Labels:                copyMap(opts.Labels),
		CreatedAt:             time.Now().UnixNano(),
	}); err != nil {
		return SandboxCheckpointResult{}, err
	}
	keepCanonicalRecord = true
	return SandboxCheckpointResult{
		TemplateID: info.BootIndexDigest,
	}, nil
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
	pulled, err := conchimage.PullBootIndex(ctx, s.Containerd, conchimage.RegistryPullOptions{
		Reference: reference,
		PlainHTTP: opts.PlainHTTP,
		Username:  opts.Username,
		Password:  opts.Password,
	})
	if err != nil {
		return TemplatePullResult{}, fmt.Errorf("pull template boot index %s: %w", reference, translateTemplateArtifactError(err))
	}
	info := pulled.Info
	origin := conchtemplate.OriginImage
	bootMode := conchtemplate.BootModeCold
	if info.Resume {
		origin = conchtemplate.OriginCheckpoint
		bootMode = conchtemplate.BootModeResume
	}
	entry, err := s.Templates.Create(ctx, conchtemplate.Entry{
		Origin:          origin,
		BootMode:        bootMode,
		BootIndexDigest: info.BootIndexDigest,
		SourceRef:       reference,
		Labels:          opts.Labels,
	})
	if err != nil {
		if !errors.Is(err, conchtemplate.ErrAlreadyExists) {
			s.cleanupCanonicalTemplateRecord(ctx, info.BootIndexDigest)
		}
		return TemplatePullResult{}, err
	}
	return TemplatePullResult{
		TemplateID: entry.BootIndexDigest,
		BuildRef:   pulled.BuildRef,
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
	id := strings.TrimSpace(opts.TemplateID)
	if id == "" {
		return conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("template_id is required"))
	}
	remoteReference := strings.TrimSpace(opts.RemoteReference)
	if remoteReference == "" {
		return conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("remote template reference is required"))
	}
	rec, err := s.Templates.Get(ctx, id)
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
	id := strings.TrimSpace(opts.TemplateID)
	if id == "" {
		return conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("template_id is required"))
	}
	rec, err := s.Templates.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get template %s: %w", id, err)
	}
	if err := conchimage.UnpackBootIndex(ctx, s.Containerd, rec.BootIndexDigest); err != nil {
		return fmt.Errorf("unpack template %s: %w", id, translateTemplateArtifactError(err))
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
	keepCanonicalRecord := false
	defer func() {
		if !keepCanonicalRecord {
			s.cleanupCanonicalTemplateRecord(ctx, result.bootIndexDigest)
		}
	}()
	info, err := conchimage.InspectBootIndex(ctx, s.Containerd, result.bootIndexDigest)
	if err != nil {
		return TemplateCreateResult{}, fmt.Errorf("validate published boot index: %w", err)
	}
	if info.BootIndexDigest != result.bootIndexDigest {
		return TemplateCreateResult{}, fmt.Errorf(
			"validated boot index digest %s does not match published digest %s",
			info.BootIndexDigest,
			result.bootIndexDigest,
		)
	}
	bootMode := conchtemplate.BootModeCold
	if info.Resume {
		bootMode = conchtemplate.BootModeResume
	}
	entry, err := s.Templates.Create(ctx, conchtemplate.Entry{
		Origin:          conchtemplate.OriginImage,
		BootMode:        bootMode,
		BootIndexDigest: info.BootIndexDigest,
		SourceRef:       source,
		Labels:          opts.Labels,
	})
	if err != nil {
		return TemplateCreateResult{}, err
	}
	keepCanonicalRecord = true
	return TemplateCreateResult{
		TemplateID: entry.BootIndexDigest,
		BuildRef:   result.buildRef,
	}, nil
}

type templateBuildResult struct {
	bootIndexDigest string
	buildRef        string
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
	if err := conchimage.SetImageKindLabel(sourceCtx, s.Containerd.ImageService(), sourceImage.Name(), conchimage.ImageKindOCIImage); err != nil {
		return templateBuildResult{}, fmt.Errorf("label rootfs source image: %w", err)
	}

	buildID, err := NewID()
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

	published, err := conchimage.PublishBootIndex(ctx, s.Containerd, conchimage.PublishBootIndexOptions{
		RootfsImageName: converted.ImageName,
		KernelPath:      opts.KernelPath,
		InitrdPath:      opts.InitrdPath,
	})
	if err != nil {
		return templateBuildResult{}, fmt.Errorf("publish boot image: %w", err)
	}

	// The converted image name is only a build-time handle. Once the Boot Index
	// has been published, its digest-derived canonical image record is the GC
	// root for the complete descriptor closure.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := conchimage.Remove(cleanupCtx, s.Containerd, runtimeapi.RemoveImageOptions{
		ImageName: converted.ImageName,
	}); err != nil {
		ulog.GetLogger().Warn("failed to remove temporary converted rootfs image",
			ulog.F("image", converted.ImageName),
			ulog.F("error", err))
	}

	return templateBuildResult{
		bootIndexDigest: published.BootIndexDigest,
		buildRef:        published.BuildRef,
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

func (s *Service) GetTemplate(ctx context.Context, id string) (runtimeapi.TemplateRecord, error) {
	if s == nil || s.Templates == nil {
		return runtimeapi.TemplateRecord{}, fmt.Errorf("template store is not configured")
	}
	rec, err := s.Templates.Get(ctx, id)
	if err != nil {
		return runtimeapi.TemplateRecord{}, err
	}
	return publicTemplateRecord(rec), nil
}

func (s *Service) RemoveTemplate(ctx context.Context, id string) error {
	if s == nil || s.Templates == nil {
		return fmt.Errorf("template store is not configured")
	}
	if s.Containerd == nil {
		return fmt.Errorf("containerd client is required")
	}
	if _, err := s.Templates.Get(ctx, id); err != nil {
		if !errors.Is(err, conchtemplate.ErrNotFound) {
			return err
		}
	} else if err := s.Templates.Delete(ctx, id); err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return conchimage.RemoveCanonicalBootIndexRecord(cleanupCtx, s.Containerd, id)
}

func publicTemplateRecord(entry conchtemplate.Entry) runtimeapi.TemplateRecord {
	buildRef, _ := conchimage.CanonicalTemplateRef(entry.BootIndexDigest)
	return runtimeapi.TemplateRecord{
		TemplateID:       entry.BootIndexDigest,
		Origin:           string(entry.Origin),
		BootMode:         string(entry.BootMode),
		ParentTemplateID: entry.ParentBootIndexDigest,
		SourceSandboxID:  entry.SourceSandboxID,
		SourceRef:        entry.SourceRef,
		BuildRef:         buildRef,
		Labels:           copyMap(entry.Labels),
		CreatedAt:        entry.CreatedAt,
	}
}

func (s *Service) cleanupCanonicalTemplateRecord(ctx context.Context, bootIndexDigest string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := conchimage.RemoveCanonicalBootIndexRecord(cleanupCtx, s.Containerd, bootIndexDigest); err != nil {
		ulog.GetLogger().Warn("failed to roll back canonical template image record",
			ulog.F("boot_index_digest", bootIndexDigest),
			ulog.F("error", err))
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

func (s *Service) upsertSandbox(ctx context.Context, rec state.SandboxRecord) error {
	if s == nil || s.Store == nil {
		return nil
	}
	return s.Store.UpsertSandbox(ctx, rec)
}

func (s *Service) getSandbox(ctx context.Context, id string) (state.SandboxRecord, error) {
	if s == nil || s.Store == nil {
		return state.SandboxRecord{}, fmt.Errorf("sandbox state store is not configured")
	}
	rec, err := s.Store.GetSandbox(ctx, id)
	if errors.Is(err, state.ErrNotFound) {
		return state.SandboxRecord{}, sandbox.ErrNotFound.Wrap(err)
	}
	return rec, err
}

func translateSandboxError(err error) error {
	if err == nil {
		return nil
	}
	var appErr *apperror.Error
	if errors.As(err, &appErr) {
		return err
	}
	switch {
	case errors.Is(err, agentprotocol.ErrInvalidEnvironment):
		return sandbox.ErrInvalidEnvironment.Wrap(err)
	case errors.Is(err, agentprotocol.ErrPayloadTooLarge):
		return sandbox.ErrInitializationTooLarge.Wrap(err)
	default:
		return err
	}
}

func translateTemplateArtifactError(err error) error {
	if errors.Is(err, conchimage.ErrInvalidArgument) || errors.Is(err, conchimage.ErrInvalidContent) {
		return conchtemplate.ErrInvalidArtifact.Wrap(err)
	}
	return err
}

// combineOperationErrors preserves the primary operation's application
// classification. Secondary rollback or cleanup errors remain available as
// causes when a primary classification exists, but can never accidentally turn
// an otherwise internal failure into a client error.
func combineOperationErrors(primary error, secondary ...error) error {
	if primary == nil {
		return errors.Join(secondary...)
	}
	additional := errors.Join(secondary...)
	if additional == nil {
		return primary
	}
	var appErr *apperror.Error
	if errors.As(primary, &appErr) {
		return appErr.WrapMessage(errors.Join(primary, additional), appErr.PublicMessage())
	}
	return fmt.Errorf("%w; additional operation failures: %v", primary, additional)
}

func NewID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
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
