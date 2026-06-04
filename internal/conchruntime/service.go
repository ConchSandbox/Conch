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
	"syscall"
	"time"

	digestpkg "github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	imageSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/image"
	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	runtimeSnapshot "github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
)

type SandboxOps interface {
	Create(sandbox.SandboxCreateRequest) (sandbox.SandboxCreateResult, error)
	Delete(sandbox.SandboxDeleteRequest) error
	Pause(sandbox.SandboxPauseRequest) (string, error)
}

type ImageOps interface {
	Pull(context.Context, imageSvc.PullRequest) (map[string]string, error)
	Push(context.Context, imageSvc.PushRequest) error
	List(context.Context, imageSvc.ListRequest) ([]imageSvc.Meta, error)
	Remove(context.Context, imageSvc.RemoveRequest) error
	Unpack(context.Context, imageSvc.UnpackRequest) (map[string]string, error)
	ImportArchive(context.Context, io.Reader, imageSvc.ImportArchiveRequest) (imageSvc.ImportArchiveResponse, error)
	ExportArchive(context.Context, io.Writer, imageSvc.ExportArchiveRequest) error
	PrepareRootfsSource(context.Context, imageSvc.PrepareRootfsSourceRequest) (imageSvc.PrepareRootfsSourceResponse, error)
	ConvertRootfsToErofs(context.Context, imageSvc.ConvertRootfsToErofsRequest) (imageSvc.ConvertRootfsToErofsResponse, error)
}

type SnapshotOps interface {
	LinkVM(context.Context, snapshotSvc.LinkVMRequest) error
	List(context.Context, snapshotSvc.ListRequest) ([]snapshotSvc.Meta, error)
	Remove(context.Context, snapshotSvc.RemoveRequest) error
	Info(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Meta, error)
	Chain(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Chain, error)
}

type Service struct {
	Sandbox          SandboxOps
	Image            ImageOps
	Snapshot         SnapshotOps
	Store            state.Store
	DefaultNamespace string
	SandboxDefaults  SandboxDefaults
}

func New(sandboxOps SandboxOps, imageOps ImageOps, store state.Store, defaultNamespace ...string) *Service {
	namespace := "default"
	if len(defaultNamespace) > 0 && strings.TrimSpace(defaultNamespace[0]) != "" {
		namespace = strings.TrimSpace(defaultNamespace[0])
	}
	return &Service{
		Sandbox:          sandboxOps,
		Image:            imageOps,
		Store:            store,
		DefaultNamespace: namespace,
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
		Namespace:   namespace,
		SnapshotId:  opts.SnapshotID,
		ImageName:   opts.ImageName,
		UseSnapshot: opts.UseSnapshot,
		VmmName:     opts.VMMName,
		SandboxId:   opts.SandboxID,
		LeaseID:     opts.LeaseID,
		VcpuNum:     opts.VCPUNum,
		VcpuMax:     opts.VCPUMax,
		RamMB:       opts.RamMB,
		AgentToken:  agentToken,
	}

	createdAt := time.Now().UnixNano()
	createResult, err := s.Sandbox.Create(req)
	rec := state.SandboxRecord{
		PodSandboxID:    opts.PodSandboxID,
		ConchSandboxID:  opts.SandboxID,
		Namespace:       namespace,
		PodNamespace:    firstNonEmpty(opts.PodNamespace, opts.Namespace),
		Name:            opts.Name,
		UID:             opts.UID,
		Attempt:         opts.Attempt,
		State:           state.SandboxReady,
		CreatedAt:       createdAt,
		Labels:          copyMap(opts.Labels),
		Annotations:     copyMap(opts.Annotations),
		RuntimeHandler:  opts.RuntimeHandler,
		LeaseID:         opts.LeaseID,
		ImageName:       opts.ImageName,
		SnapshotID:      opts.SnapshotID,
		IP:              createResult.IP,
		VMMName:         opts.VMMName,
		VCPUNum:         opts.VCPUNum,
		RamMB:           opts.RamMB,
		VMMPID:          createResult.VMMPID,
		VMMSocketPath:   createResult.VMMSocketPath,
		VsockCID:        createResult.VsockCID,
		VsockSocketPath: createResult.VsockSocketPath,
		NetworkSlotKey:  createResult.NetworkSlotKey,
		NetworkNS:       createResult.NetworkNS,
		RootfsKey:       createResult.RootfsKey,
		MemKey:          createResult.MemKey,
		ParentRootfsID:  createResult.ParentRootfsID,
		ParentMemID:     createResult.ParentMemID,
		ParentVMID:      createResult.ParentVMID,
		RootfsMount:     createResult.RootfsMount,
		MemMount:        createResult.MemMount,
		VMMount:         createResult.VMMount,
		SnapshotRootDir: createResult.RootDir,
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
	if err := s.upsertSandboxRuntimeState(ctx, rec, createResult); err != nil {
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

func (s *Service) applySandboxDefaults(opts *SandboxCreateOptions) {
	if s == nil || opts == nil {
		return
	}
	defaults := s.SandboxDefaults
	if opts.ImageName == "" && opts.SnapshotID == "" {
		opts.ImageName = defaults.ImageName
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

func (s *Service) StopSandbox(ctx context.Context, namespace, podSandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
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
	runtimeMissing := err != nil && strings.Contains(err.Error(), "not found")
	if runtimeMissing {
		err = nil
		if stateFound {
			err = terminateRecordedVMM(rec)
		}
	}
	if err == nil {
		_ = s.deleteSandboxRuntimeState(ctx, namespace, sandboxID)
	}
	if !stateFound {
		return err
	}
	if rec.PodSandboxID == "" {
		rec.PodSandboxID = podSandboxID
		rec.ConchSandboxID = sandboxID
		rec.Namespace = namespace
	}
	rec.StoppedAt = time.Now().UnixNano()
	rec.State = state.SandboxStopped
	if err != nil {
		rec.State = state.SandboxUnknown
		rec.LastError = err.Error()
	}
	_ = s.upsertSandbox(ctx, rec)
	return err
}

func (s *Service) RemoveSandbox(ctx context.Context, namespace, podSandboxID string) error {
	err := s.StopSandbox(ctx, namespace, podSandboxID)
	if err == nil && s != nil && s.Store != nil {
		_ = s.Store.DeleteSandbox(ctx, podSandboxID)
	}
	return err
}

func (s *Service) PauseSandbox(ctx context.Context, namespace, podSandboxID string) (string, error) {
	if s == nil || s.Sandbox == nil {
		return "", fmt.Errorf("sandbox service is not configured")
	}
	rec, _ := s.getSandbox(ctx, podSandboxID)
	sandboxID := rec.ConchSandboxID
	if sandboxID == "" {
		sandboxID = podSandboxID
	}
	if namespace == "" {
		namespace = rec.Namespace
	}
	namespace = s.normalizeNamespace(namespace)
	snapshotID, err := s.Sandbox.Pause(sandbox.SandboxPauseRequest{Namespace: namespace, SandboxId: sandboxID})
	if rec.PodSandboxID != "" {
		rec.SnapshotID = snapshotID
		rec.StoppedAt = time.Now().UnixNano()
		rec.State = state.SandboxStopped
		if err != nil {
			rec.State = state.SandboxUnknown
			rec.LastError = err.Error()
		} else {
			_ = s.deleteSandboxRuntimeState(ctx, namespace, sandboxID)
		}
		_ = s.upsertSandbox(ctx, rec)
	}
	return snapshotID, err
}

func (s *Service) PullImage(ctx context.Context, opts PullImageOptions) (PullImageResult, error) {
	if s == nil || s.Image == nil {
		return PullImageResult{}, fmt.Errorf("image service is not configured")
	}
	refs, err := s.PullImageRequest(ctx, imageSvc.PullRequest{
		ImageName:          opts.ImageName,
		Namespace:          opts.Namespace,
		Username:           opts.Username,
		Password:           opts.Password,
		DefaultKernelImage: opts.DefaultKernelImage,
	})
	if err != nil {
		return PullImageResult{}, err
	}
	return PullImageResult{Refs: refs}, nil
}

func (s *Service) PullImageRequest(ctx context.Context, req imageSvc.PullRequest) (map[string]string, error) {
	if s == nil || s.Image == nil {
		return nil, fmt.Errorf("image service is not configured")
	}
	return s.Image.Pull(ctx, req)
}

func (s *Service) PushImageRequest(ctx context.Context, req imageSvc.PushRequest) error {
	if s == nil || s.Image == nil {
		return fmt.Errorf("image service is not configured")
	}
	return s.Image.Push(ctx, req)
}

func (s *Service) ListImages(ctx context.Context, opts runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error) {
	items, err := s.ListImageRequest(ctx, imageSvc.ListRequest{
		Namespace: opts.Namespace,
		Filters:   opts.Filters,
	})
	if err != nil {
		return nil, err
	}
	out := make([]runtimeapi.ImageRecord, 0, len(items))
	for _, item := range items {
		out = append(out, runtimeapi.ImageRecord{
			Name:            item.Name,
			TargetDigest:    item.TargetDigest,
			RepoDigests:     imageRepoDigests(item.Name, item.TargetDigest),
			TargetMediaType: item.TargetMediaType,
			Size:            item.Size,
			Kind:            firstNonEmpty(strings.TrimSpace(item.Kind), imageKindFromLabels(item.Labels)),
			Labels:          item.Labels,
		})
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

func (s *Service) ListImageRequest(ctx context.Context, req imageSvc.ListRequest) ([]imageSvc.Meta, error) {
	if s == nil || s.Image == nil {
		return nil, fmt.Errorf("image service is not configured")
	}
	return s.Image.List(ctx, req)
}

func (s *Service) RemoveImage(ctx context.Context, opts runtimeapi.RemoveImageOptions) error {
	return s.RemoveImageRequest(ctx, imageSvc.RemoveRequest{
		Namespace:   opts.Namespace,
		ImageName:   opts.ImageName,
		Synchronous: opts.Synchronous,
	})
}

func (s *Service) RemoveImageRequest(ctx context.Context, req imageSvc.RemoveRequest) error {
	if s == nil || s.Image == nil {
		return fmt.Errorf("image service is not configured")
	}
	return s.Image.Remove(ctx, req)
}

func (s *Service) UnpackImage(ctx context.Context, req imageSvc.UnpackRequest) (map[string]string, error) {
	if s == nil || s.Image == nil {
		return nil, fmt.Errorf("image service is not configured")
	}
	return s.Image.Unpack(ctx, req)
}

func (s *Service) ImportImageArchive(ctx context.Context, reader io.Reader, req imageSvc.ImportArchiveRequest) (imageSvc.ImportArchiveResponse, error) {
	if s == nil || s.Image == nil {
		return imageSvc.ImportArchiveResponse{}, fmt.Errorf("image service is not configured")
	}
	return s.Image.ImportArchive(ctx, reader, req)
}

func (s *Service) ExportImageArchive(ctx context.Context, writer io.Writer, req imageSvc.ExportArchiveRequest) error {
	if s == nil || s.Image == nil {
		return fmt.Errorf("image service is not configured")
	}
	return s.Image.ExportArchive(ctx, writer, req)
}

func (s *Service) PrepareRootfsSource(ctx context.Context, req imageSvc.PrepareRootfsSourceRequest) (imageSvc.PrepareRootfsSourceResponse, error) {
	if s == nil || s.Image == nil {
		return imageSvc.PrepareRootfsSourceResponse{}, fmt.Errorf("image service is not configured")
	}
	return s.Image.PrepareRootfsSource(ctx, req)
}

func (s *Service) ConvertRootfsToErofs(ctx context.Context, req imageSvc.ConvertRootfsToErofsRequest) (imageSvc.ConvertRootfsToErofsResponse, error) {
	if s == nil || s.Image == nil {
		return imageSvc.ConvertRootfsToErofsResponse{}, fmt.Errorf("image service is not configured")
	}
	return s.Image.ConvertRootfsToErofs(ctx, req)
}

func (s *Service) LinkSnapshotVM(ctx context.Context, req snapshotSvc.LinkVMRequest) error {
	if s == nil || s.Snapshot == nil {
		return fmt.Errorf("snapshot service is not configured")
	}
	return s.Snapshot.LinkVM(ctx, req)
}

func (s *Service) ListSnapshotRequest(ctx context.Context, req snapshotSvc.ListRequest) ([]snapshotSvc.Meta, error) {
	if s == nil || s.Snapshot == nil {
		return nil, fmt.Errorf("snapshot service is not configured")
	}
	return s.Snapshot.List(ctx, req)
}

func (s *Service) RemoveSnapshotRequest(ctx context.Context, req snapshotSvc.RemoveRequest) error {
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

func (s *Service) upsertSandboxRuntimeState(ctx context.Context, rec state.SandboxRecord, createResult sandbox.SandboxCreateResult) error {
	if s == nil || s.Store == nil {
		return nil
	}
	if rec.RootfsKey != "" || rec.MemKey != "" {
		if err := s.Store.UpsertSnapshotRuntime(ctx, state.SnapshotRuntimeRecord{
			Namespace:      rec.Namespace,
			SandboxID:      rec.ConchSandboxID,
			RootfsKey:      rec.RootfsKey,
			MemKey:         rec.MemKey,
			ParentRootfsID: rec.ParentRootfsID,
			ParentMemID:    rec.ParentMemID,
			ParentVMID:     rec.ParentVMID,
			LeaseID:        rec.LeaseID,
			RootfsMount:    rec.RootfsMount,
			MemMount:       rec.MemMount,
			VMMount:        rec.VMMount,
			RootDir:        createResult.RootDir,
			MemSize:        createResult.MemSize,
			State:          state.SandboxReady,
		}); err != nil {
			return err
		}
	}
	if rec.ParentVMID != "" && rec.VMMount != "" {
		if err := s.upsertViewRuntimeState(ctx, rec.Namespace, rec.ConchSandboxID, rec.ParentVMID, common.SnapshotMountVM, rec.VMMount, rec.LeaseID); err != nil {
			return err
		}
	}
	if createResult.Resume && rec.ParentRootfsID != "" && rec.RootfsMount != "" {
		if err := s.upsertViewRuntimeState(ctx, rec.Namespace, rec.ConchSandboxID, rec.ParentRootfsID, common.SnapshotMountRootfs, rec.RootfsMount, rec.LeaseID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) upsertViewRuntimeState(ctx context.Context, namespace, sandboxID, parentID, mountKind, mountPoint, leaseID string) error {
	viewSnapshotKey := runtimeSnapshot.SharedViewSnapshotKey(mountKind, parentID)
	var aliasKey string
	switch mountKind {
	case common.SnapshotMountRootfs:
		aliasKey = runtimeSnapshot.RootfsViewAliasKey(sandboxID)
	case common.SnapshotMountMem:
		aliasKey = runtimeSnapshot.MemViewAliasKey(sandboxID)
	case common.SnapshotMountVM:
		aliasKey = runtimeSnapshot.VMViewAliasKey(sandboxID)
	default:
		return fmt.Errorf("unknown view mount kind %q", mountKind)
	}
	if err := s.Store.UpsertViewSnapshot(ctx, state.ViewSnapshotRecord{
		Namespace:        namespace,
		ParentSnapshotID: parentID,
		ViewSnapshotKey:  viewSnapshotKey,
		LeaseID:          leaseID,
		MountPoint:       mountPoint,
		RefCount:         1,
		State:            state.SandboxReady,
	}); err != nil {
		return err
	}
	return s.Store.UpsertViewAlias(ctx, state.ViewAliasRecord{
		Namespace:        namespace,
		AliasKey:         aliasKey,
		SandboxID:        sandboxID,
		ParentSnapshotID: parentID,
		MountKind:        mountKind,
	})
}

func (s *Service) deleteSandboxRuntimeState(ctx context.Context, namespace, sandboxID string) error {
	if s == nil || s.Store == nil || sandboxID == "" {
		return nil
	}
	var errs []error
	if err := s.Store.DeleteSnapshotRuntime(ctx, namespace, sandboxID); err != nil {
		errs = append(errs, err)
	}
	aliasKeys := []string{
		runtimeSnapshot.RootfsViewAliasKey(sandboxID),
		runtimeSnapshot.MemViewAliasKey(sandboxID),
		runtimeSnapshot.VMViewAliasKey(sandboxID),
	}
	parents := make(map[string]struct{})
	aliases, err := s.Store.ListViewAliases(ctx)
	if err != nil {
		errs = append(errs, err)
	} else {
		targetAliases := make(map[string]struct{}, len(aliasKeys))
		for _, aliasKey := range aliasKeys {
			targetAliases[aliasKey] = struct{}{}
		}
		for _, rec := range aliases {
			if rec.Namespace != namespace {
				continue
			}
			if _, ok := targetAliases[rec.AliasKey]; ok && rec.ParentSnapshotID != "" {
				parents[rec.ParentSnapshotID] = struct{}{}
			}
		}
	}
	for _, aliasKey := range aliasKeys {
		if err := s.Store.DeleteViewAlias(ctx, namespace, aliasKey); err != nil {
			errs = append(errs, err)
		}
	}
	if len(parents) > 0 {
		if err := s.reconcileViewSnapshotRefs(ctx, namespace, parents); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) reconcileViewSnapshotRefs(ctx context.Context, namespace string, parents map[string]struct{}) error {
	aliases, err := s.Store.ListViewAliases(ctx)
	if err != nil {
		return err
	}
	remainingRefs := make(map[string]int, len(parents))
	for _, rec := range aliases {
		if rec.Namespace != namespace {
			continue
		}
		if _, ok := parents[rec.ParentSnapshotID]; ok {
			remainingRefs[rec.ParentSnapshotID]++
		}
	}
	views, err := s.Store.ListViewSnapshots(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, rec := range views {
		if rec.Namespace != namespace {
			continue
		}
		if _, ok := parents[rec.ParentSnapshotID]; !ok {
			continue
		}
		if remainingRefs[rec.ParentSnapshotID] == 0 {
			if err := s.Store.DeleteViewSnapshot(ctx, namespace, rec.ParentSnapshotID); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		rec.RefCount = remainingRefs[rec.ParentSnapshotID]
		if err := s.Store.UpsertViewSnapshot(ctx, rec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func terminateRecordedVMM(rec state.SandboxRecord) error {
	if rec.VMMPID <= 0 {
		return nil
	}
	matched, err := recordedVMMProcessMatches(rec)
	if err != nil {
		return err
	}
	if !matched {
		return fmt.Errorf("refuse to signal vmm pid %d: process does not match sandbox %s", rec.VMMPID, rec.ConchSandboxID)
	}
	if err := syscall.Kill(rec.VMMPID, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("send SIGTERM to vmm pid %d: %w", rec.VMMPID, err)
	}
	return nil
}

func recordedVMMProcessMatches(rec state.SandboxRecord) (bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", rec.VMMPID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("read vmm pid %d cmdline: %w", rec.VMMPID, err)
	}
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")
	for _, marker := range []string{rec.ConchSandboxID, rec.VMMSocketPath, rec.VsockSocketPath} {
		if marker != "" && strings.Contains(cmdline, marker) {
			return true, nil
		}
	}
	return false, nil
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
