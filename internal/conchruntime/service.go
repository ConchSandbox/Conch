package conchruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	imageSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/image"
	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	"github.com/openeuler/Conch/internal/sandbox"
)

type SandboxOps interface {
	Create(sandbox.SandboxCreateRequest) (string, error)
	Delete(sandbox.SandboxDeleteRequest) error
	Pause(sandbox.SandboxPauseRequest) (string, error)
}

type ImageOps interface {
	Pull(context.Context, imageSvc.PullRequest) (map[string]string, error)
	Push(context.Context, imageSvc.PushRequest) error
	Unpack(context.Context, imageSvc.UnpackRequest) (map[string]string, error)
	ImportArchive(context.Context, io.Reader, imageSvc.ImportArchiveRequest) (imageSvc.ImportArchiveResponse, error)
	ExportArchive(context.Context, io.Writer, imageSvc.ExportArchiveRequest) error
	PrepareRootfsSource(context.Context, imageSvc.PrepareRootfsSourceRequest) (imageSvc.PrepareRootfsSourceResponse, error)
	ConvertRootfsToErofs(context.Context, imageSvc.ConvertRootfsToErofsRequest) (imageSvc.ConvertRootfsToErofsResponse, error)
}

type SnapshotOps interface {
	LinkVM(context.Context, snapshotSvc.LinkVMRequest) error
	Info(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Meta, error)
	Chain(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Chain, error)
}

type Service struct {
	Sandbox          SandboxOps
	Image            ImageOps
	Snapshot         SnapshotOps
	DefaultNamespace string
	SandboxDefaults  SandboxDefaults
}

func New(sandboxOps SandboxOps, imageOps ImageOps, defaultNamespace ...string) *Service {
	namespace := "default"
	if len(defaultNamespace) > 0 && strings.TrimSpace(defaultNamespace[0]) != "" {
		namespace = strings.TrimSpace(defaultNamespace[0])
	}
	return &Service{
		Sandbox:          sandboxOps,
		Image:            imageOps,
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
	s.applySandboxDefaults(&opts)

	ip, err := s.Sandbox.Create(sandbox.SandboxCreateRequest{
		Namespace:   namespace,
		SnapshotId:  opts.SnapshotID,
		ImageName:   opts.ImageName,
		UseSnapshot: opts.UseSnapshot,
		VmmName:     opts.VMMName,
		SandboxId:   opts.SandboxID,
		VcpuNum:     opts.VCPUNum,
		VcpuMax:     opts.VCPUMax,
		RamMB:       opts.RamMB,
	})
	if err != nil {
		return SandboxCreateResult{}, err
	}
	return SandboxCreateResult{
		PodSandboxID: opts.PodSandboxID,
		SandboxID:    opts.SandboxID,
		Namespace:    namespace,
		IP:           ip,
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

func (s *Service) StopSandbox(ctx context.Context, namespace, sandboxID string) error {
	if s == nil || s.Sandbox == nil {
		return fmt.Errorf("sandbox service is not configured")
	}
	namespace = s.normalizeNamespace(namespace)
	return s.Sandbox.Delete(sandbox.SandboxDeleteRequest{Namespace: namespace, SandboxId: sandboxID})
}

func (s *Service) RemoveSandbox(ctx context.Context, namespace, sandboxID string) error {
	return s.StopSandbox(ctx, namespace, sandboxID)
}

func (s *Service) PauseSandbox(ctx context.Context, namespace, sandboxID string) (string, error) {
	if s == nil || s.Sandbox == nil {
		return "", fmt.Errorf("sandbox service is not configured")
	}
	namespace = s.normalizeNamespace(namespace)
	return s.Sandbox.Pause(sandbox.SandboxPauseRequest{Namespace: namespace, SandboxId: sandboxID})
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
