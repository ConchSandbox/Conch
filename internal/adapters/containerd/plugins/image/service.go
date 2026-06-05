package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/conchplugins"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/snapshot/common"
)

var (
	ErrInvalidRequest      = errors.New("invalid image request")
	ErrOCIConversionFailed = errors.New("oci conversion failed")
)

const SnapshotLabelVMSnapshot = "conch/snapshotter/vm-snapshot"

const (
	defaultRegistryResponseHeaderTimeout = 10 * time.Minute
	registryTimeoutEnv                   = "CONCH_REGISTRY_TIMEOUT"
)

type PullRequest struct {
	ImageName              string `json:"image_name"`
	Namespace              string `json:"namespace,omitempty"`
	PlainHTTP              bool   `json:"plain_http,omitempty"`
	Username               string `json:"username,omitempty"`
	Password               string `json:"password,omitempty"`
	DefaultKernelImage     string `json:"default_kernel_image,omitempty"`
	KernelPlainHTTP        bool   `json:"kernel_plain_http,omitempty"`
	KernelRegistryUsername string `json:"kernel_username,omitempty"`
	KernelRegistryPassword string `json:"kernel_password,omitempty"`
}

type PushRequest struct {
	LocalImage      string `json:"local_image"`
	RemoteImage     string `json:"remote_image"`
	Namespace       string `json:"namespace,omitempty"`
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	RegistryTimeout string `json:"registry_timeout,omitempty"`
}

type UnpackRequest struct {
	ImageName string `json:"image_name"`
	Namespace string `json:"namespace,omitempty"`
}

type ImportArchiveRequest struct {
	Namespace   string `json:"namespace,omitempty"`
	ImportedTag string `json:"imported_tag,omitempty"`
}

type ImportArchiveResponse struct {
	SnapshotKey string `json:"snapshot_key"`
	ImageName   string `json:"image_name"`
}

type ExportArchiveRequest struct {
	Namespace string `json:"namespace,omitempty"`
	ImageName string `json:"image_name"`
}

type ListRequest struct {
	Namespace string   `json:"namespace,omitempty"`
	Filters   []string `json:"filters,omitempty"`
}

type RemoveRequest struct {
	Namespace   string `json:"namespace,omitempty"`
	ImageName   string `json:"image_name"`
	Synchronous bool   `json:"synchronous,omitempty"`
}

type Meta struct {
	Name            string            `json:"name"`
	TargetDigest    string            `json:"target_digest"`
	TargetMediaType string            `json:"target_media_type"`
	Size            int64             `json:"size,omitempty"`
	Kind            string            `json:"kind,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	CreatedAt       time.Time         `json:"created_at,omitempty"`
	UpdatedAt       time.Time         `json:"updated_at,omitempty"`
}

type PrepareRootfsSourceRequest struct {
	Source      string `json:"source"`
	Namespace   string `json:"namespace,omitempty"`
	TargetImage string `json:"target_image,omitempty"`
	PlainHTTP   bool   `json:"plain_http,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
}

type PrepareRootfsSourceResponse struct {
	ImageName      string `json:"image_name"`
	ManifestDigest string `json:"manifest_digest"`
}

type ConvertRootfsToErofsRequest = erofsconvert.ConvertRootfsRequest
type ConvertRootfsToErofsResponse = erofsconvert.ConvertRootfsResult
type ErofsLayer = erofsconvert.ErofsLayer

type Service struct {
	client    *containerdclient.Client
	converter erofsconvert.RootfsErofsConverter
}

func New(client *containerdclient.Client) *Service {
	svc := &Service{client: client}
	if client != nil {
		svc.converter = erofsconvert.NewToolkitConverter(client)
	}
	return svc
}

func (s *Service) SetRootfsErofsConverter(converter erofsconvert.RootfsErofsConverter) {
	s.converter = converter
}

func (s *Service) Pull(ctx context.Context, req PullRequest) (map[string]string, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("image service has no containerd client")
	}
	if req.ImageName == "" {
		return nil, fmt.Errorf("%w: image_name is required", ErrInvalidRequest)
	}

	ns := req.Namespace
	if ns == "" {
		ns = s.client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	pullCtx := namespaces.WithNamespace(ctx, ns)
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.PlainHTTP,
		Credentials: func(string) (string, string, error) {
			return req.Username, req.Password, nil
		},
	})
	pullOpts := []containerd.RemoteOpt{
		containerd.WithResolver(resolver),
	}
	if _, err := s.client.Pull(pullCtx, req.ImageName, pullOpts...); err != nil {
		return nil, fmt.Errorf("pull image %s: %w", req.ImageName, err)
	}

	if _, err := s.client.Fetch(pullCtx, req.ImageName, containerd.WithResolver(resolver)); err != nil {
		return nil, fmt.Errorf("fetch all Conch image content: %w", err)
	}

	results, err := conchimage.UnpackAllSubImages(pullCtx, s.client.Client, req.ImageName)
	if err == nil {
		return results, nil
	}
	if !errors.Is(err, conchimage.ErrMissingSandbox) || req.DefaultKernelImage == "" {
		return nil, fmt.Errorf("unpack pulled image: %w", err)
	}

	kernelResolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.KernelPlainHTTP,
		Credentials: func(string) (string, string, error) {
			return req.KernelRegistryUsername, req.KernelRegistryPassword, nil
		},
	})
	if _, err := s.client.Pull(pullCtx, req.DefaultKernelImage, containerd.WithResolver(kernelResolver)); err != nil {
		return nil, fmt.Errorf("pull default kernel image %s: %w", req.DefaultKernelImage, err)
	}
	if _, err := s.client.Fetch(pullCtx, req.DefaultKernelImage, containerd.WithResolver(kernelResolver)); err != nil {
		return nil, fmt.Errorf("fetch default kernel image %s content: %w", req.DefaultKernelImage, err)
	}
	results, err = conchimage.UnpackAllSubImagesWithDefaultSandbox(pullCtx, s.client.Client, req.ImageName, req.DefaultKernelImage)
	if err != nil {
		return nil, fmt.Errorf("unpack pulled image with default kernel image %s: %w", req.DefaultKernelImage, err)
	}
	return results, nil
}

func (s *Service) PrepareRootfsSource(ctx context.Context, req PrepareRootfsSourceRequest) (PrepareRootfsSourceResponse, error) {
	if s == nil || s.client == nil {
		return PrepareRootfsSourceResponse{}, fmt.Errorf("image service has no containerd client")
	}
	if req.Source == "" {
		return PrepareRootfsSourceResponse{}, fmt.Errorf("%w: source is required", ErrInvalidRequest)
	}
	ns := req.Namespace
	if ns == "" {
		ns = s.client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	sourceCtx := namespaces.WithNamespace(ctx, ns)

	img, err := s.client.GetImage(sourceCtx, req.Source)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return PrepareRootfsSourceResponse{}, fmt.Errorf("lookup rootfs source image %s: %w", req.Source, err)
		}
		resolver := docker.NewResolver(docker.ResolverOptions{
			PlainHTTP: req.PlainHTTP,
			Credentials: func(string) (string, string, error) {
				return req.Username, req.Password, nil
			},
		})
		pulled, err := s.client.Pull(sourceCtx, req.Source, containerd.WithResolver(resolver))
		if err != nil {
			return PrepareRootfsSourceResponse{}, fmt.Errorf("pull rootfs source image %s: %w", req.Source, err)
		}
		img = pulled
	}

	imageName := img.Name()
	if req.TargetImage != "" && req.TargetImage != imageName {
		alias := images.Image{Name: req.TargetImage, Target: img.Target()}
		if _, err := s.client.ImageService().Create(sourceCtx, alias); err != nil {
			if errdefs.IsAlreadyExists(err) {
				if _, updateErr := s.client.ImageService().Update(sourceCtx, alias, "target"); updateErr != nil {
					return PrepareRootfsSourceResponse{}, fmt.Errorf("update rootfs source image alias %s: %w", req.TargetImage, updateErr)
				}
			} else {
				return PrepareRootfsSourceResponse{}, fmt.Errorf("create rootfs source image alias %s: %w", req.TargetImage, err)
			}
		}
		imageName = req.TargetImage
	}

	return PrepareRootfsSourceResponse{
		ImageName:      imageName,
		ManifestDigest: img.Target().Digest.String(),
	}, nil
}

func (s *Service) Push(ctx context.Context, req PushRequest) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("image service has no containerd client")
	}
	if req.LocalImage == "" {
		return fmt.Errorf("%w: local_image is required", ErrInvalidRequest)
	}
	if req.RemoteImage == "" {
		return fmt.Errorf("%w: remote_image is required", ErrInvalidRequest)
	}
	ns := req.Namespace
	if ns == "" {
		ns = s.client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	pushCtx := namespaces.WithNamespace(ctx, ns)
	img, err := s.client.GetImage(pushCtx, req.LocalImage)
	if err != nil {
		return fmt.Errorf("lookup image %s: %w", req.LocalImage, err)
	}
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.PlainHTTP,
		Client:    registryHTTPClient(req.RegistryTimeout),
		Credentials: func(string) (string, string, error) {
			return req.Username, req.Password, nil
		},
	})
	if err := s.client.Push(pushCtx, req.RemoteImage, img.Target(), containerd.WithResolver(resolver), containerd.WithMaxConcurrentUploadedLayers(1)); err != nil {
		return fmt.Errorf("push image %s -> %s: %w", req.LocalImage, req.RemoteImage, err)
	}
	return nil
}

func (s *Service) List(ctx context.Context, req ListRequest) ([]Meta, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("image service has no containerd client")
	}
	ns := req.Namespace
	if ns == "" {
		ns = s.client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	listCtx := namespaces.WithNamespace(ctx, ns)
	items, err := s.client.ImageService().List(listCtx, req.Filters...)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	out := make([]Meta, 0, len(items))
	for _, item := range items {
		kind := imageKindFromLabels(item.Labels)
		if kind == "" {
			kind = s.classifyImageKind(listCtx, item)
		}
		out = append(out, Meta{
			Name:            item.Name,
			TargetDigest:    item.Target.Digest.String(),
			TargetMediaType: item.Target.MediaType,
			Size:            item.Target.Size,
			Kind:            kind,
			Labels:          item.Labels,
			CreatedAt:       item.CreatedAt,
			UpdatedAt:       item.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Service) Remove(ctx context.Context, req RemoveRequest) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("image service has no containerd client")
	}
	if req.ImageName == "" {
		return fmt.Errorf("%w: image_name is required", ErrInvalidRequest)
	}
	ns := req.Namespace
	if ns == "" {
		ns = s.client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	removeCtx := namespaces.WithNamespace(ctx, ns)
	opts := []images.DeleteOpt{}
	if req.Synchronous {
		opts = append(opts, images.SynchronousDelete())
	}
	if err := s.client.ImageService().Delete(removeCtx, req.ImageName, opts...); err != nil {
		return fmt.Errorf("remove image %s: %w", req.ImageName, err)
	}
	return nil
}

func registryHTTPClient(registryTimeoutValue string) *http.Client {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultClient
	}
	cloned := tr.Clone()
	cloned.ResponseHeaderTimeout = resolveRegistryResponseHeaderTimeout(registryTimeoutValue)
	return &http.Client{Transport: cloned}
}

func imageKindFromLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	for _, key := range []string{"io.conch.kind", "conch.io/kind", "kind"} {
		if kind := labels[key]; kind != "" {
			return kind
		}
	}
	return ""
}

func (s *Service) classifyImageKind(ctx context.Context, item images.Image) string {
	if s == nil || s.client == nil {
		return ""
	}
	switch item.Target.MediaType {
	case ocispec.MediaTypeImageIndex:
		return s.classifyIndexKind(ctx, item.Target)
	default:
		return inferComponentKindFromName(item.Name)
	}
}

func (s *Service) classifyIndexKind(ctx context.Context, target ocispec.Descriptor) string {
	indexData, err := content.ReadBlob(ctx, s.client.ContentStore(), target)
	if err != nil {
		return ""
	}
	var index ocispec.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return ""
	}
	return classifyConchIndexKind(index)
}

func classifyConchIndexKind(index ocispec.Index) string {
	hasRootfs := false
	hasSandbox := false
	hasMem := false
	for _, manifest := range index.Manifests {
		switch manifest.Annotations["io.conch.kind"] {
		case conchimage.KindRootfs:
			hasRootfs = true
		case conchimage.KindSandbox:
			hasSandbox = true
		case conchimage.KindMemSnapshot:
			hasMem = true
		}
	}
	if hasRootfs && hasSandbox {
		if hasMem {
			return "sandbox-snapshot"
		}
		return "sandbox-base"
	}
	return ""
}

func inferComponentKindFromName(name string) string {
	switch {
	case strings.Contains(name, "/rootfs-component:") || strings.HasSuffix(name, "-rootfs"):
		return conchimage.KindRootfs
	case strings.Contains(name, "/sandbox-component:") || strings.HasSuffix(name, "-sandbox"):
		return conchimage.KindSandbox
	case strings.Contains(name, "/mem-snapshot-component:") || strings.HasSuffix(name, "-mem"):
		return conchimage.KindMemSnapshot
	default:
		return ""
	}
}

func resolveRegistryResponseHeaderTimeout(registryTimeoutValue string) time.Duration {
	if raw := strings.TrimSpace(registryTimeoutValue); raw != "" {
		return parseRegistryResponseHeaderTimeout(raw)
	}
	if raw := strings.TrimSpace(os.Getenv(registryTimeoutEnv)); raw != "" {
		return parseRegistryResponseHeaderTimeout(raw)
	}
	return defaultRegistryResponseHeaderTimeout
}

func parseRegistryResponseHeaderTimeout(raw string) time.Duration {
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return defaultRegistryResponseHeaderTimeout
	}
	return timeout
}

func (s *Service) Unpack(ctx context.Context, req UnpackRequest) (map[string]string, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("image service has no containerd client")
	}
	if req.ImageName == "" {
		return nil, fmt.Errorf("%w: image_name is required", ErrInvalidRequest)
	}

	ns := req.Namespace
	if ns == "" {
		ns = s.client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	unpackCtx := namespaces.WithNamespace(ctx, ns)
	results, err := conchimage.UnpackAllSubImages(unpackCtx, s.client.Client, req.ImageName)
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Service) ImportArchive(ctx context.Context, archive io.Reader, req ImportArchiveRequest) (ImportArchiveResponse, error) {
	if s == nil || s.client == nil {
		return ImportArchiveResponse{}, fmt.Errorf("image service has no containerd client")
	}
	if archive == nil {
		return ImportArchiveResponse{}, fmt.Errorf("%w: archive is required", ErrInvalidRequest)
	}

	ns := req.Namespace
	if ns == "" {
		ns = s.client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	importCtx := namespaces.WithNamespace(ctx, ns)
	importOpts := []containerd.ImportOpt{}
	if req.ImportedTag != "" {
		importOpts = append(importOpts, containerd.WithIndexName(req.ImportedTag))
	}
	importedImages, err := s.client.Import(importCtx, archive, importOpts...)
	if err != nil {
		return ImportArchiveResponse{}, fmt.Errorf("containerd import failed: %w", err)
	}
	if len(importedImages) == 0 {
		return ImportArchiveResponse{}, fmt.Errorf("no images were imported")
	}

	snapshotter := s.client.SnapshotService("erofs")
	var finalSnapshotKey string
	var finalImageName string
	for _, imgInfo := range reorderImportedImages(importedImages, req.ImportedTag) {
		if imgInfo.Target.MediaType == ocispec.MediaTypeImageIndex {
			if err := conchimage.ValidateConchImageIndex(importCtx, s.client.Client, imgInfo.Name); err == nil {
				snapshotMap, err := conchimage.UnpackAllSubImages(importCtx, s.client.Client, imgInfo.Name)
				if err != nil {
					return ImportArchiveResponse{}, fmt.Errorf("failed to unpack Conch image index %s: %w", imgInfo.Name, err)
				}
				finalSnapshotKey = snapshotMap[conchimage.KindRootfs]
				finalImageName = imgInfo.Name
				break
			}
		}
		img := containerd.NewImage(s.client.Client, imgInfo)
		if err := img.Unpack(importCtx, "erofs"); err != nil {
			return ImportArchiveResponse{}, fmt.Errorf("failed to unpack image %s: %w", imgInfo.Name, err)
		}

		diffIDs, err := img.RootFS(importCtx)
		if err != nil {
			return ImportArchiveResponse{}, fmt.Errorf("failed to get rootfs: %w", err)
		}
		finalSnapshotKey = identity.ChainID(diffIDs).String()
		finalImageName = imgInfo.Name
		if _, err := snapshotter.Stat(importCtx, finalSnapshotKey); err != nil {
			continue
		}
		break
	}

	if finalSnapshotKey == "" {
		return ImportArchiveResponse{}, fmt.Errorf("no snapshot key generated")
	}
	return ImportArchiveResponse{
		SnapshotKey: finalSnapshotKey,
		ImageName:   finalImageName,
	}, nil
}

func (s *Service) ExportArchive(ctx context.Context, w io.Writer, req ExportArchiveRequest) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("image service has no containerd client")
	}
	if w == nil {
		return fmt.Errorf("%w: archive writer is required", ErrInvalidRequest)
	}
	if req.ImageName == "" {
		return fmt.Errorf("%w: image_name is required", ErrInvalidRequest)
	}

	ns := req.Namespace
	if ns == "" {
		ns = s.client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	exportCtx := namespaces.WithNamespace(ctx, ns)
	if _, err := s.client.ImageService().Get(exportCtx, req.ImageName); err != nil {
		return fmt.Errorf("lookup image %s: %w", req.ImageName, err)
	}
	if err := s.client.Export(exportCtx, w, archive.WithImage(s.client.ImageService(), req.ImageName)); err != nil {
		return fmt.Errorf("export image %s: %w", req.ImageName, err)
	}
	return nil
}

func (s *Service) ConvertRootfsToErofs(ctx context.Context, req ConvertRootfsToErofsRequest) (ConvertRootfsToErofsResponse, error) {
	if s == nil || s.client == nil {
		return ConvertRootfsToErofsResponse{}, fmt.Errorf("image service has no containerd client")
	}
	if s.converter == nil {
		return ConvertRootfsToErofsResponse{}, fmt.Errorf("rootfs erofs converter is not configured")
	}

	if req.Namespace == "" {
		req.Namespace = s.client.DefaultNamespace()
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	normalized, err := erofsconvert.NormalizeRequest(req)
	if err != nil {
		return ConvertRootfsToErofsResponse{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	result, err := s.converter.Convert(ctx, normalized)
	if err != nil {
		return ConvertRootfsToErofsResponse{}, fmt.Errorf("convert rootfs to erofs: %w", err)
	}
	convertCtx := namespaces.WithNamespace(ctx, normalized.Namespace)
	imgInfo, err := s.client.ImageService().Get(convertCtx, result.ImageName)
	if err != nil {
		return ConvertRootfsToErofsResponse{}, fmt.Errorf("lookup converted image: %w", err)
	}
	if err := containerd.NewImage(s.client.Client, imgInfo).Unpack(convertCtx, "erofs"); err != nil {
		return ConvertRootfsToErofsResponse{}, fmt.Errorf("unpack converted rootfs with erofs snapshotter: %w", err)
	}
	snapshotKey, err := conchimage.GetSnapshotID(ctx, s.client, normalized.Namespace, result.ImageName)
	if err != nil {
		return ConvertRootfsToErofsResponse{}, fmt.Errorf("resolve converted rootfs snapshot key: %w", err)
	}
	if _, err := s.client.SnapshotService("erofs").Update(convertCtx, snapshots.Info{
		Name: snapshotKey,
		Labels: map[string]string{
			common.SnapshotLabelRootfsImage:    result.ImageName,
			common.SnapshotLabelRootfsManifest: result.ManifestDigest,
		},
	}, "labels."+common.SnapshotLabelRootfsImage, "labels."+common.SnapshotLabelRootfsManifest); err != nil {
		return ConvertRootfsToErofsResponse{}, fmt.Errorf("label converted rootfs snapshot: %w", err)
	}
	result.SnapshotKey = snapshotKey
	return result, nil
}

func reorderImportedImages(imported []images.Image, preferredName string) []images.Image {
	if preferredName == "" || len(imported) <= 1 {
		return imported
	}
	out := make([]images.Image, 0, len(imported))
	for _, img := range imported {
		if img.Name == preferredName {
			out = append(out, img)
		}
	}
	for _, img := range imported {
		if img.Name != preferredName {
			out = append(out, img)
		}
	}
	return out
}

var (
	readyMu sync.Mutex
	readyCh chan<- *Service
)

func SetReadyChannel(ch chan<- *Service) {
	readyMu.Lock()
	defer readyMu.Unlock()
	readyCh = ch
}

func publishReady(svc *Service) {
	readyMu.Lock()
	ch := readyCh
	readyMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- svc:
	default:
	}
}

type daemonClientProvider interface {
	DaemonClient() *containerdclient.Client
}

func init() {
	registry.Register(&plugin.Registration{
		Type: conchplugins.ImageServicePluginType,
		ID:   conchplugins.ImageServiceID,
		Requires: []plugin.Type{
			conchplugins.HostPluginType,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			inst, err := ic.GetByID(conchplugins.HostPluginType, conchplugins.HostPluginID)
			if err != nil {
				return nil, err
			}
			provider, ok := inst.(daemonClientProvider)
			if !ok {
				return nil, fmt.Errorf("%s does not provide daemon client", conchplugins.HostPluginURI)
			}
			svc := New(provider.DaemonClient())
			publishReady(svc)
			return svc, nil
		},
	})
}
