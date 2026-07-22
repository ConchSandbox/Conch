package conchruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	digestpkg "github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/internal/volume"
	"github.com/openeuler/Conch/pkg/ulog"
)

type SandboxOps interface {
	Create(sandbox.SandboxCreateRequest) (sandbox.SandboxCreateResult, error)
	Delete(sandbox.SandboxDeleteRequest) error
	Suspend(sandbox.SandboxLifecycleRequest) error
	Resume(sandbox.SandboxLifecycleRequest) error
	Checkpoint(sandbox.SandboxCheckpointRequest) (sandbox.SandboxCheckpointResult, error)
}

type ImageOps interface {
	Pull(context.Context, runtimeapi.PullImageOptions) (runtimeapi.PullImageResult, error)
	Push(context.Context, runtimeapi.PushImageOptions) error
	List(context.Context, runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error)
	Remove(context.Context, runtimeapi.RemoveImageOptions) error
	Unpack(context.Context, runtimeapi.UnpackImageOptions) (map[string]string, error)
	ImportArchive(context.Context, io.Reader, runtimeapi.ImportImageArchiveOptions) (runtimeapi.ImportImageArchiveResult, error)
	ExportArchive(context.Context, io.Writer, runtimeapi.ExportImageArchiveOptions) error
}

// TemplateBootIndexOps groups image operations that produce, inspect, or
// distribute Boot Indexes for Templates. These still run through the containerd
// image service, but they are conceptually separate from generic OCI image
// lifecycle operations.
type TemplateBootIndexOps interface {
	PushBootIndex(context.Context, conchimage.PushBootIndexOptions) error
	PrepareRootfsSource(context.Context, conchimage.PrepareRootfsSourceOptions) (conchimage.PrepareRootfsSourceResult, error)
	PublishBootImage(context.Context, conchimage.PublishBootImageOptions) (conchimage.PublishBootImageResult, error)
	InspectBootIndex(ctx context.Context, namespace, bootIndexDigest string) (conchimage.BootIndexInfo, error)
	InspectBootIndexReference(ctx context.Context, namespace, reference string) (conchimage.BootIndexInfo, error)
	PublishCheckpointBootImage(context.Context, conchimage.PublishCheckpointBootImageOptions) (conchimage.PublishCheckpointBootImageResult, error)
	ConvertRootfsToErofs(context.Context, erofsconvert.ConvertRootfsRequest) (erofsconvert.ConvertRootfsResult, error)
}

type SnapshotOps interface {
	List(context.Context, snapshotSvc.ListRequest) ([]snapshotSvc.Meta, error)
	Remove(context.Context, snapshotSvc.RemoveRequest) error
	Info(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Meta, error)
	Chain(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Chain, error)
}

type Service struct {
	Sandbox           SandboxOps
	Image             ImageOps
	TemplateBootIndex TemplateBootIndexOps
	Snapshot          SnapshotOps
	Store             state.Store
	Templates         conchtemplate.Store
	DefaultNamespace  string
	SandboxDefaults   SandboxDefaults
	lifecycleLocks    sandboxLifecycleLocks
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

func New(sandboxOps SandboxOps, imageOps ImageOps, templateBootIndexOps TemplateBootIndexOps, store state.Store, defaultNamespace ...string) *Service {
	namespace := "default"
	if len(defaultNamespace) > 0 && strings.TrimSpace(defaultNamespace[0]) != "" {
		namespace = strings.TrimSpace(defaultNamespace[0])
	}
	return &Service{
		Sandbox:           sandboxOps,
		Image:             imageOps,
		TemplateBootIndex: templateBootIndexOps,
		Store:             store,
		Templates:         conchtemplate.NewStore(store),
		DefaultNamespace:  namespace,
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
	if opts.SandboxID == "" {
		opts.SandboxID = opts.PodSandboxID
	}
	if opts.PodSandboxID == "" {
		opts.PodSandboxID = opts.SandboxID
	}
	if opts.PodSandboxID == "" {
		id, err := NewID()
		if err != nil {
			return SandboxCreateResult{}, err
		}
		opts.PodSandboxID = id
		opts.SandboxID = id
	}
	unlock := s.lifecycleLocks.lock(opts.PodSandboxID)
	defer unlock()
	namespace := s.normalizeNamespace(opts.Namespace)
	if opts.LeaseID == "" {
		opts.LeaseID = containerdclient.RuntimeLeaseID(namespace)
	}
	s.applySandboxDefaults(&opts)
	agentToken, err := sandbox.GenerateAgentToken()
	if err != nil {
		return SandboxCreateResult{}, err
	}

	req := sandbox.SandboxCreateRequest{
		Namespace:    namespace,
		TemplateID:   opts.TemplateID,
		VmmName:      opts.VMMName,
		SandboxId:    opts.SandboxID,
		LeaseID:      opts.LeaseID,
		VcpuNum:      opts.VCPUNum,
		VcpuMax:      opts.VCPUMax,
		RamMB:        opts.RamMB,
		AgentToken:   agentToken,
		VolumeMounts: opts.VolumeMounts,
	}

	createdAt := time.Now().UnixNano()
	createResult, err := s.Sandbox.Create(req)
	rec := state.SandboxRecord{
		PodSandboxID:                  opts.PodSandboxID,
		ConchSandboxID:                opts.SandboxID,
		Namespace:                     namespace,
		PodNamespace:                  firstNonEmpty(opts.PodNamespace, opts.Namespace),
		Name:                          opts.Name,
		UID:                           opts.UID,
		Attempt:                       opts.Attempt,
		State:                         state.SandboxReady,
		CreatedAt:                     createdAt,
		Labels:                        copyMap(opts.Labels),
		Annotations:                   copyMap(opts.Annotations),
		RuntimeHandler:                opts.RuntimeHandler,
		LeaseID:                       opts.LeaseID,
		SourceTemplateID:              opts.TemplateID,
		SourceBootIndexDigest:         createResult.BootIndexDigest,
		CheckpointHeadTemplateID:      opts.TemplateID,
		CheckpointHeadBootIndexDigest: createResult.BootIndexDigest,
		IP:                            createResult.IP,
		VMMName:                       opts.VMMName,
		VCPUNum:                       opts.VCPUNum,
		RamMB:                         opts.RamMB,
		VMMPID:                        createResult.VMMPID,
		VMMSocketPath:                 createResult.VMMSocketPath,
		VsockCID:                      createResult.VsockCID,
		VsockSocketPath:               createResult.VsockSocketPath,
		NetworkSlotKey:                createResult.NetworkSlotKey,
		NetworkNS:                     createResult.NetworkNS,
		RootfsKey:                     createResult.RootfsKey,
		MemKey:                        createResult.MemKey,
		RootfsMount:                   createResult.RootfsMount,
		RootfsPmemPaths:               append([]string(nil), createResult.RootfsPmemPaths...),
		MemMount:                      createResult.MemMount,
		VMMount:                       createResult.VMMount,
		SnapshotRootDir:               createResult.RootDir,
		VolumeDevices:                 volumeDevicesToState(createResult.VolumeDevices),
	}
	if err != nil {
		rec.State = state.SandboxUnknown
		rec.LastError = err.Error()
		_ = s.upsertSandbox(ctx, rec)
		return SandboxCreateResult{}, err
	}
	if err := s.upsertSandbox(ctx, rec); err != nil {
		return SandboxCreateResult{}, err
	}
	return SandboxCreateResult{
		PodSandboxID: opts.PodSandboxID,
		SandboxID:    opts.SandboxID,
		Namespace:    namespace,
		IP:           createResult.IP,
		AgentToken:   createResult.AgentToken,
	}, nil
}

func volumeDevicesToState(devices []volume.Device) []state.VolumeDevice {
	if len(devices) == 0 {
		return nil
	}
	out := make([]state.VolumeDevice, 0, len(devices))
	for _, device := range devices {
		out = append(out, state.VolumeDevice{
			SandboxID:  device.SandboxID,
			Namespace:  device.Namespace,
			Backend:    device.Backend,
			Tag:        device.Tag,
			Socket:     device.Socket,
			VolumeDir:  device.VolumeDir,
			ConfigPath: device.ConfigPath,
			PID:        device.PID,
			StartTime:  device.StartTime,
		})
	}
	return out
}

func (s *Service) applySandboxDefaults(opts *SandboxCreateOptions) {
	if s == nil || opts == nil {
		return
	}
	defaults := s.SandboxDefaults
	if opts.TemplateID == "" {
		opts.TemplateID = defaults.TemplateID
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

func (s *Service) RemoveSandbox(ctx context.Context, namespace, podSandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	unlock := s.lifecycleLocks.lock(podSandboxID)
	defer unlock()
	rec, recErr := s.getSandbox(ctx, podSandboxID)
	stateFound := recErr == nil
	if recErr != nil && !errors.Is(recErr, state.ErrNotFound) {
		return fmt.Errorf("get sandbox state: %w", recErr)
	}
	sandboxID := rec.ConchSandboxID
	if sandboxID == "" {
		sandboxID = podSandboxID
	}
	if namespace == "" {
		namespace = rec.Namespace
	}
	namespace = s.normalizeNamespace(namespace)
	err := s.Sandbox.Delete(sandbox.SandboxDeleteRequest{Namespace: namespace, SandboxId: sandboxID})
	if err != nil && strings.Contains(err.Error(), "not found") {
		err = nil
	}
	if err != nil {
		if stateFound {
			rec.State = state.SandboxUnknown
			rec.LastError = err.Error()
			_ = s.upsertSandbox(ctx, rec)
		}
		return err
	}
	if s.Store != nil {
		return s.Store.DeleteSandbox(ctx, podSandboxID)
	}
	return nil
}

func (s *Service) SuspendSandbox(ctx context.Context, namespace, podSandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	unlock := s.lifecycleLocks.lock(podSandboxID)
	defer unlock()
	rec, _ := s.getSandbox(ctx, podSandboxID)
	sandboxID := rec.ConchSandboxID
	if sandboxID == "" {
		sandboxID = podSandboxID
	}
	if namespace == "" {
		namespace = rec.Namespace
	}
	namespace = s.normalizeNamespace(namespace)
	err := s.Sandbox.Suspend(sandbox.SandboxLifecycleRequest{Namespace: namespace, SandboxId: sandboxID})
	if rec.PodSandboxID != "" {

		rec.State = state.SandboxSuspended
		if err != nil {
			rec.State = state.SandboxUnknown
			rec.LastError = err.Error()
		}
		_ = s.upsertSandbox(ctx, rec)
	}
	return err
}

func (s *Service) ResumeSandbox(ctx context.Context, namespace, podSandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	unlock := s.lifecycleLocks.lock(podSandboxID)
	defer unlock()
	rec, _ := s.getSandbox(ctx, podSandboxID)
	sandboxID := rec.ConchSandboxID
	if sandboxID == "" {
		sandboxID = podSandboxID
	}
	if namespace == "" {
		namespace = rec.Namespace
	}
	namespace = s.normalizeNamespace(namespace)
	err := s.Sandbox.Resume(sandbox.SandboxLifecycleRequest{Namespace: namespace, SandboxId: sandboxID})
	if rec.PodSandboxID != "" {
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
	unlock := s.lifecycleLocks.lock(opts.PodSandboxID)
	defer unlock()
	rec, err := s.getSandbox(ctx, opts.PodSandboxID)
	if err != nil {
		return SandboxCheckpointResult{}, err
	}
	if s.Image == nil {
		return SandboxCheckpointResult{}, fmt.Errorf("image service is not configured")
	}
	if s.TemplateBootIndex == nil {
		return SandboxCheckpointResult{}, fmt.Errorf("template boot index service is not configured")
	}
	if s.Templates == nil {
		return SandboxCheckpointResult{}, fmt.Errorf("template store is not configured")
	}
	sandboxID := rec.ConchSandboxID
	if sandboxID == "" {
		sandboxID = opts.PodSandboxID
	}
	namespace := opts.Namespace
	if namespace == "" {
		namespace = rec.Namespace
	}
	namespace = s.normalizeNamespace(namespace)
	if recordNamespace := s.normalizeNamespace(rec.Namespace); recordNamespace != namespace {
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox %s belongs to namespace %s, not %s", sandboxID, recordNamespace, namespace)
	}
	if len(rec.VolumeDevices) > 0 {
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox %s has volume mounts, checkpoint is not supported", sandboxID)
	}
	parentTemplateID := firstNonEmpty(rec.CheckpointHeadTemplateID, rec.SourceTemplateID)
	if parentTemplateID == "" {
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox %s has no checkpoint head template", sandboxID)
	}
	parentBootIndexDigest := firstNonEmpty(rec.CheckpointHeadBootIndexDigest, rec.SourceBootIndexDigest)
	if parentBootIndexDigest == "" {
		parent, getErr := s.Templates.Get(ctx, parentTemplateID)
		if getErr != nil {
			return SandboxCheckpointResult{}, fmt.Errorf("resolve checkpoint head template %s: %w", parentTemplateID, getErr)
		}
		parentBootIndexDigest = strings.TrimSpace(parent.BootIndexDigest)
	}
	if parentBootIndexDigest == "" {
		return SandboxCheckpointResult{}, fmt.Errorf("sandbox %s checkpoint head %s has no boot index digest", sandboxID, parentTemplateID)
	}

	templateRecord, err := s.Templates.Create(ctx, conchtemplate.CreateRequest{
		Origin:           state.TemplateOriginCheckpoint,
		Namespace:        namespace,
		ParentTemplateID: parentTemplateID,
		SourceSandboxID:  sandboxID,
		Labels:           opts.Labels,
	})
	if err != nil {
		return SandboxCheckpointResult{}, err
	}

	captured, err := s.Sandbox.Checkpoint(sandbox.SandboxCheckpointRequest{
		Namespace: namespace,
		SandboxId: sandboxID,
	})
	if err != nil {
		_ = s.Templates.MarkFailed(ctx, templateRecord.ID, err)
		return SandboxCheckpointResult{}, err
	}
	defer os.RemoveAll(captured.MemRootPath)

	bootIndexTag := "localhost/conch/template:" + templateRecord.ID
	published, err := s.TemplateBootIndex.PublishCheckpointBootImage(ctx, conchimage.PublishCheckpointBootImageOptions{
		Namespace:             namespace,
		SourceBootIndexDigest: parentBootIndexDigest,
		BootIndexTag:          bootIndexTag,
		MemRoot:               captured.MemRootPath,
		VMMName:               captured.VMMName,
		MemorySizeMB:          captured.MemorySizeMB,
	})
	if err != nil {
		_ = s.Templates.MarkFailed(ctx, templateRecord.ID, err)
		return SandboxCheckpointResult{}, err
	}
	if err := s.Templates.PublishCheckpoint(ctx, state.CheckpointPublication{
		TemplateID:                  templateRecord.ID,
		PodSandboxID:                rec.PodSandboxID,
		BootIndexDigest:             published.BootIndexDigest,
		BootMode:                    state.TemplateBootModeResume,
		BuildRef:                    published.ImageName,
		ExpectedHeadTemplateID:      parentTemplateID,
		ExpectedHeadBootIndexDigest: parentBootIndexDigest,
	}); err != nil {
		_ = s.Templates.MarkFailed(ctx, templateRecord.ID, err)
		return SandboxCheckpointResult{}, err
	}
	return SandboxCheckpointResult{
		TemplateID:      templateRecord.ID,
		BootIndexDigest: published.BootIndexDigest,
	}, nil
}

func (s *Service) PullImage(ctx context.Context, opts PullImageOptions) (PullImageResult, error) {
	if s == nil || s.Image == nil {
		return PullImageResult{}, fmt.Errorf("image service is not configured")
	}
	result, err := s.Image.Pull(ctx, opts)
	if err != nil {
		return PullImageResult{}, err
	}
	return result, nil
}

func (s *Service) PushImage(ctx context.Context, opts runtimeapi.PushImageOptions) error {
	if s == nil || s.Image == nil {
		return fmt.Errorf("image service is not configured")
	}
	return s.Image.Push(ctx, opts)
}

func (s *Service) ListImages(ctx context.Context, opts runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error) {
	if s == nil || s.Image == nil {
		return nil, fmt.Errorf("image service is not configured")
	}
	items, err := s.Image.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := make([]runtimeapi.ImageRecord, 0, len(items))
	for _, item := range items {
		item.RepoDigests = imageRepoDigests(item.Name, item.TargetDigest)
		item.Kind = firstNonEmpty(strings.TrimSpace(item.Kind), imageKindFromLabels(item.Labels))
		out = append(out, item)
	}
	return out, nil
}

func imageRepoDigests(name, digest string) []string {
	name = strings.TrimSpace(name)
	digest = strings.TrimSpace(digest)
	if name == "" || digest == "" {
		return nil
	}
	if isDigestOnlyRef(name) {
		return nil
	}
	base := name
	if repo, _, ok := strings.Cut(base, "@"); ok {
		base = repo
	} else {
		lastSlash := strings.LastIndex(base, "/")
		lastColon := strings.LastIndex(base, ":")
		if lastColon > lastSlash {
			base = base[:lastColon]
		}
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return nil
	}
	return []string{base + "@" + digest}
}

func isDigestOnlyRef(ref string) bool {
	if _, err := digestpkg.Parse(ref); err == nil {
		return true
	}
	algo, _, ok := strings.Cut(ref, ":")
	if !ok || strings.Contains(algo, "/") {
		return false
	}
	switch algo {
	case "sha256", "sha384", "sha512":
		return true
	default:
		return false
	}
}

func imageKindFromLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	if kind := strings.TrimSpace(labels["io.conch.kind"]); kind != "" {
		return kind
	}
	if kind := strings.TrimSpace(labels["kind"]); kind != "" {
		return kind
	}
	if kind := strings.TrimSpace(labels["conch.io/kind"]); kind != "" {
		return kind
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s *Service) RemoveImage(ctx context.Context, opts runtimeapi.RemoveImageOptions) error {
	if s == nil || s.Image == nil {
		return fmt.Errorf("image service is not configured")
	}
	return s.Image.Remove(ctx, opts)
}

func (s *Service) UnpackImage(ctx context.Context, req runtimeapi.UnpackImageOptions) (map[string]string, error) {
	if s == nil || s.Image == nil {
		return nil, fmt.Errorf("image service is not configured")
	}
	return s.Image.Unpack(ctx, req)
}

func (s *Service) ImportImageArchive(ctx context.Context, reader io.Reader, req runtimeapi.ImportImageArchiveOptions) (runtimeapi.ImportImageArchiveResult, error) {
	if s == nil || s.Image == nil {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("image service is not configured")
	}
	return s.Image.ImportArchive(ctx, reader, req)
}

func (s *Service) ExportImageArchive(ctx context.Context, writer io.Writer, req runtimeapi.ExportImageArchiveOptions) error {
	if s == nil || s.Image == nil {
		return fmt.Errorf("image service is not configured")
	}
	return s.Image.ExportArchive(ctx, writer, req)
}

func (s *Service) PublishBootImage(ctx context.Context, req conchimage.PublishBootImageOptions) (conchimage.PublishBootImageResult, error) {
	if s == nil || s.TemplateBootIndex == nil {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("template boot index service is not configured")
	}
	return s.TemplateBootIndex.PublishBootImage(ctx, req)
}

func (s *Service) PrepareRootfsSource(ctx context.Context, req conchimage.PrepareRootfsSourceOptions) (conchimage.PrepareRootfsSourceResult, error) {
	if s == nil || s.TemplateBootIndex == nil {
		return conchimage.PrepareRootfsSourceResult{}, fmt.Errorf("template boot index service is not configured")
	}
	return s.TemplateBootIndex.PrepareRootfsSource(ctx, req)
}

func (s *Service) ConvertRootfsToErofs(ctx context.Context, req erofsconvert.ConvertRootfsRequest) (erofsconvert.ConvertRootfsResult, error) {
	if s == nil || s.TemplateBootIndex == nil {
		return erofsconvert.ConvertRootfsResult{}, fmt.Errorf("template boot index service is not configured")
	}
	return s.TemplateBootIndex.ConvertRootfsToErofs(ctx, req)
}

// PullTemplate fetches and statically validates a registry Boot Index before
// publishing a local READY Template. Runtime boot validation belongs to
// integration tests, not the pull request path.
func (s *Service) PullTemplate(ctx context.Context, opts TemplatePullOptions) (TemplatePullResult, error) {
	if s == nil || s.Image == nil {
		return TemplatePullResult{}, fmt.Errorf("image service is required")
	}
	if s.TemplateBootIndex == nil {
		return TemplatePullResult{}, fmt.Errorf("template boot index service is not configured")
	}
	if s.Templates == nil {
		return TemplatePullResult{}, fmt.Errorf("template store is not configured")
	}
	reference := strings.TrimSpace(opts.Reference)
	if reference == "" {
		return TemplatePullResult{}, fmt.Errorf("template reference is required")
	}
	namespace := s.normalizeNamespace(opts.Namespace)
	if _, err := s.Image.Pull(ctx, runtimeapi.PullImageOptions{
		ImageName:  reference,
		Namespace:  namespace,
		PlainHTTP:  opts.PlainHTTP,
		Username:   opts.Username,
		Password:   opts.Password,
		SkipUnpack: true,
	}); err != nil {
		return TemplatePullResult{}, fmt.Errorf("pull template boot index %s: %w", reference, err)
	}
	info, err := s.TemplateBootIndex.InspectBootIndexReference(ctx, namespace, reference)
	if err != nil {
		return TemplatePullResult{}, fmt.Errorf("validate pulled template boot index %s: %w", reference, err)
	}
	origin := state.TemplateOriginImage
	bootMode := state.TemplateBootModeCold
	if info.Resume {
		origin = state.TemplateOriginCheckpoint
		bootMode = state.TemplateBootModeResume
	}
	rec, err := s.Templates.Create(ctx, conchtemplate.CreateRequest{
		Origin:    origin,
		Namespace: namespace,
		ImageName: reference,
		BuildRef:  reference,
		Labels:    opts.Labels,
	})
	if err != nil {
		return TemplatePullResult{}, err
	}
	if err := s.Templates.MarkReady(ctx, rec.ID, conchtemplate.ReadyState{
		BootIndexDigest: info.BootIndexDigest,
		BootMode:        bootMode,
		BuildRef:        reference,
	}); err != nil {
		_ = s.Templates.MarkFailed(ctx, rec.ID, err)
		return TemplatePullResult{}, err
	}
	return TemplatePullResult{
		TemplateID:      rec.ID,
		BootIndexDigest: info.BootIndexDigest,
		BuildRef:        reference,
	}, nil
}

// PushTemplate publishes the descriptor closure rooted at the READY
// Template's immutable BootIndexDigest. BuildRef is provenance only and may
// have been retargeted since the Template was created.
func (s *Service) PushTemplate(ctx context.Context, opts TemplatePushOptions) error {
	if s == nil || s.TemplateBootIndex == nil {
		return fmt.Errorf("template boot index service is required")
	}
	if s.Templates == nil {
		return fmt.Errorf("template store is not configured")
	}
	templateID := strings.TrimSpace(opts.TemplateID)
	if templateID == "" {
		return fmt.Errorf("template id is required")
	}
	remoteReference := strings.TrimSpace(opts.RemoteReference)
	if remoteReference == "" {
		return fmt.Errorf("remote template reference is required")
	}
	rec, err := s.Templates.Get(ctx, templateID)
	if err != nil {
		return err
	}
	if rec.State != state.TemplateReady {
		return fmt.Errorf("template %s is %s, want %s", rec.ID, rec.State, state.TemplateReady)
	}
	bootIndexDigest := strings.TrimSpace(rec.BootIndexDigest)
	if bootIndexDigest == "" {
		return fmt.Errorf("template %s has no boot index digest", rec.ID)
	}
	namespace := strings.TrimSpace(opts.Namespace)
	if namespace == "" {
		namespace = rec.Namespace
	}
	namespace = s.normalizeNamespace(namespace)
	if recordNamespace := s.normalizeNamespace(rec.Namespace); namespace != recordNamespace {
		return fmt.Errorf("template %s belongs to namespace %s, not %s", rec.ID, recordNamespace, namespace)
	}
	return s.TemplateBootIndex.PushBootIndex(ctx, conchimage.PushBootIndexOptions{
		BootIndexDigest: bootIndexDigest,
		RemoteReference: remoteReference,
		Namespace:       namespace,
		PlainHTTP:       opts.PlainHTTP,
		Username:        opts.Username,
		Password:        opts.Password,
		RegistryTimeout: opts.RegistryTimeout,
	})
}

func (s *Service) CreateTemplate(ctx context.Context, opts TemplateCreateOptions) (TemplateCreateResult, error) {
	if s == nil || s.TemplateBootIndex == nil {
		return TemplateCreateResult{}, fmt.Errorf("template boot index service is required")
	}
	if s.Templates == nil {
		return TemplateCreateResult{}, fmt.Errorf("template store is not configured")
	}
	namespace := s.normalizeNamespace(opts.Namespace)
	source := strings.TrimSpace(opts.Source)
	if source == "" {
		return TemplateCreateResult{}, fmt.Errorf("template source is required")
	}
	opts.Source = source
	opts.BootIndexTag = strings.TrimSpace(opts.BootIndexTag)
	templateRecord, err := s.Templates.Create(ctx, conchtemplate.CreateRequest{
		Origin:    state.TemplateOriginImage,
		Namespace: namespace,
		ImageName: source,
		BuildRef:  opts.BootIndexTag,
		Labels:    opts.Labels,
	})
	if err != nil {
		return TemplateCreateResult{}, err
	}

	result, err := s.createTemplateFromSource(ctx, namespace, templateRecord.ID, opts)
	if err != nil {
		_ = s.Templates.MarkFailed(ctx, templateRecord.ID, err)
		return TemplateCreateResult{}, err
	}
	info, err := s.TemplateBootIndex.InspectBootIndex(ctx, namespace, result.bootIndexDigest)
	if err != nil {
		_ = s.Templates.MarkFailed(ctx, templateRecord.ID, err)
		return TemplateCreateResult{}, fmt.Errorf("validate published boot index: %w", err)
	}
	bootMode := state.TemplateBootModeCold
	if info.Resume {
		bootMode = state.TemplateBootModeResume
	}
	if err := s.Templates.MarkReady(ctx, templateRecord.ID, conchtemplate.ReadyState{
		BootIndexDigest: info.BootIndexDigest,
		BootMode:        bootMode,
		BuildRef:        result.bootIndexTag,
	}); err != nil {
		_ = s.Templates.MarkFailed(ctx, templateRecord.ID, err)
		return TemplateCreateResult{}, err
	}
	return TemplateCreateResult{
		TemplateID:      templateRecord.ID,
		BootIndexDigest: result.bootIndexDigest,
		BootIndexTag:    result.bootIndexTag,
	}, nil
}

type templateBuildResult struct {
	bootIndexDigest string
	bootIndexTag    string
}

func (s *Service) createTemplateFromSource(ctx context.Context, namespace, templateID string, opts TemplateCreateOptions) (templateBuildResult, error) {
	prepared, err := s.TemplateBootIndex.PrepareRootfsSource(ctx, conchimage.PrepareRootfsSourceOptions{
		Source:    opts.Source,
		Namespace: namespace,
		PlainHTTP: opts.PlainHTTP,
		Username:  opts.Username,
		Password:  opts.Password,
	})
	if err != nil {
		return templateBuildResult{}, fmt.Errorf("prepare rootfs source: %w", err)
	}

	convertTarget := fmt.Sprintf("conch-erofs-rootfs:%s", templateID)
	converted, err := s.TemplateBootIndex.ConvertRootfsToErofs(ctx, erofsconvert.ConvertRootfsRequest{
		Namespace:   namespace,
		SourceImage: prepared.ImageName,
		TargetImage: convertTarget,
		MkfsOptions: []string{erofsconvert.DefaultMkfsOption},
		AlignBytes:  erofsconvert.DefaultAlignBytes,
	})
	if err != nil {
		return templateBuildResult{}, fmt.Errorf("convert rootfs to EROFS: %w", err)
	}

	bootIndexTag := strings.TrimSpace(opts.BootIndexTag)
	if bootIndexTag == "" {
		bootIndexTag = "localhost/conch/template:" + templateID
	}
	published, err := s.TemplateBootIndex.PublishBootImage(ctx, conchimage.PublishBootImageOptions{
		Namespace:       namespace,
		RootfsImageName: converted.ImageName,
		KernelPath:      opts.KernelPath,
		InitrdPath:      opts.InitrdPath,
		BootIndexTag:    bootIndexTag,
	})
	if err != nil {
		return templateBuildResult{}, fmt.Errorf("publish boot image: %w", err)
	}

	// The converted image name is only a build-time handle. Once the boot index
	// has been published and unpacked, the index and digest-named component image
	// records are the authoritative references to its content.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.Image.Remove(cleanupCtx, runtimeapi.RemoveImageOptions{
		Namespace: namespace,
		ImageName: converted.ImageName,
	}); err != nil {
		ulog.GetLogger().Warn("failed to remove temporary converted rootfs image",
			ulog.F("image", converted.ImageName),
			ulog.F("error", err))
	}

	return templateBuildResult{
		bootIndexDigest: published.BootIndexDigest,
		bootIndexTag:    published.ImageName,
	}, nil
}

func (s *Service) ListTemplates(ctx context.Context, opts runtimeapi.TemplateListOptions) ([]runtimeapi.TemplateRecord, error) {
	if s == nil || s.Templates == nil {
		return nil, fmt.Errorf("template store is not configured")
	}
	items, err := s.Templates.List(ctx, conchtemplate.Filter{
		Namespace: strings.TrimSpace(opts.Namespace),
		Origin:    strings.TrimSpace(opts.Origin),
		BootMode:  strings.TrimSpace(opts.BootMode),
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
	return s.Templates.Delete(ctx, id)
}

func publicTemplateRecord(rec state.TemplateRecord) runtimeapi.TemplateRecord {
	return runtimeapi.TemplateRecord{
		ID:               rec.ID,
		Origin:           rec.Origin,
		BootMode:         conchtemplate.BootMode(rec),
		BootIndexDigest:  rec.BootIndexDigest,
		Namespace:        rec.Namespace,
		State:            rec.State,
		ParentTemplateID: rec.ParentTemplateID,
		SourceSandboxID:  rec.SourceSandboxID,
		ImageName:        rec.ImageName,
		BuildRef:         rec.BuildRef,
		Labels:           copyMap(rec.Labels),
		CreatedAt:        rec.CreatedAt,
		UpdatedAt:        rec.UpdatedAt,
		LastError:        rec.LastError,
	}
}

func (s *Service) ListSnapshots(ctx context.Context, req snapshotSvc.ListRequest) ([]snapshotSvc.Meta, error) {
	if s == nil || s.Snapshot == nil {
		return nil, fmt.Errorf("snapshot service is not configured")
	}
	return s.Snapshot.List(ctx, req)
}

func (s *Service) RemoveSnapshot(ctx context.Context, req snapshotSvc.RemoveRequest) error {
	if s == nil || s.Snapshot == nil {
		return fmt.Errorf("snapshot service is not configured")
	}
	return s.Snapshot.Remove(ctx, req)
}

func (s *Service) SnapshotInfo(ctx context.Context, req snapshotSvc.InfoRequest) (snapshotSvc.Meta, error) {
	if s == nil || s.Snapshot == nil {
		return snapshotSvc.Meta{}, fmt.Errorf("snapshot service is not configured")
	}
	return s.Snapshot.Info(ctx, req)
}

func (s *Service) SnapshotChain(ctx context.Context, req snapshotSvc.InfoRequest) (snapshotSvc.Chain, error) {
	if s == nil || s.Snapshot == nil {
		return snapshotSvc.Chain{}, fmt.Errorf("snapshot service is not configured")
	}
	return s.Snapshot.Chain(ctx, req)
}

func (s *Service) CreateContainer(ctx context.Context, opts ContainerCreateOptions) (ContainerCreateResult, error) {
	if opts.ContainerID == "" {
		id, err := NewID()
		if err != nil {
			return ContainerCreateResult{}, err
		}
		opts.ContainerID = id
	}
	rec := state.ContainerRecord{
		ContainerID:  opts.ContainerID,
		PodSandboxID: opts.PodSandboxID,
		Name:         opts.Name,
		State:        state.ContainerCreated,
		CreatedAt:    time.Now().UnixNano(),
		Image:        opts.Image,
		ImageRef:     opts.ImageRef,
		Command:      append([]string(nil), opts.Command...),
		Args:         append([]string(nil), opts.Args...),
		LogPath:      opts.LogPath,
		Labels:       copyMap(opts.Labels),
		Annotations:  copyMap(opts.Annotations),
	}
	if s != nil && s.Store != nil {
		if err := s.Store.UpsertContainer(ctx, rec); err != nil {
			return ContainerCreateResult{}, err
		}
	}
	return ContainerCreateResult{ContainerID: opts.ContainerID}, nil
}

func (s *Service) SetContainerState(ctx context.Context, containerID, next string) error {
	if s == nil || s.Store == nil {
		return nil
	}
	rec, err := s.Store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	now := time.Now().UnixNano()
	rec.State = next
	switch next {
	case state.ContainerRunning:
		rec.StartedAt = now
	case state.ContainerExited:
		rec.FinishedAt = now
	}
	return s.Store.UpsertContainer(ctx, rec)
}

func (s *Service) upsertSandbox(ctx context.Context, rec state.SandboxRecord) error {
	if s == nil || s.Store == nil {
		return nil
	}
	return s.Store.UpsertSandbox(ctx, rec)
}

func (s *Service) getSandbox(ctx context.Context, id string) (state.SandboxRecord, error) {
	if s == nil || s.Store == nil {
		return state.SandboxRecord{}, state.ErrNotFound
	}
	return s.Store.GetSandbox(ctx, id)
}

func (s *Service) normalizeNamespace(namespace string) string {
	if ns := strings.TrimSpace(namespace); ns != "" {
		return ns
	}
	if s != nil && strings.TrimSpace(s.DefaultNamespace) != "" {
		return strings.TrimSpace(s.DefaultNamespace)
	}
	return "default"
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
