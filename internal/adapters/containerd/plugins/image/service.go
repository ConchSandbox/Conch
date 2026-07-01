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
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/snapshot/common"
)

const (
	defaultRegistryResponseHeaderTimeout = 10 * time.Minute
	registryTimeoutEnv                   = "CONCH_REGISTRY_TIMEOUT"
)

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

func (s *Service) Pull(ctx context.Context, req runtimeapi.PullImageOptions) (runtimeapi.PullImageResult, error) {
	if s == nil || s.client == nil {
		return runtimeapi.PullImageResult{}, fmt.Errorf("image service has no containerd client")
	}
	if req.ImageName == "" {
		return runtimeapi.PullImageResult{}, fmt.Errorf("%w: image_name is required", conchimage.ErrInvalidRequest)
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
		return runtimeapi.PullImageResult{}, fmt.Errorf("pull image %s: %w", req.ImageName, err)
	}

	if _, err := s.client.Fetch(pullCtx, req.ImageName, containerd.WithResolver(resolver)); err != nil {
		return runtimeapi.PullImageResult{}, fmt.Errorf("fetch all Conch image content: %w", err)
	}

	results, err := conchimage.UnpackAllSubImages(pullCtx, s.client.Client, req.ImageName)
	if err == nil {
		return runtimeapi.PullImageResult{Refs: results}, nil
	}
	if !errors.Is(err, conchimage.ErrMissingSandbox) || req.DefaultKernelImage == "" {
		return runtimeapi.PullImageResult{}, fmt.Errorf("unpack pulled image: %w", err)
	}

	kernelResolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: req.KernelPlainHTTP,
		Credentials: func(string) (string, string, error) {
			return req.KernelRegistryUsername, req.KernelRegistryPassword, nil
		},
	})
	if _, err := s.client.Pull(pullCtx, req.DefaultKernelImage, containerd.WithResolver(kernelResolver)); err != nil {
		return runtimeapi.PullImageResult{}, fmt.Errorf("pull default kernel image %s: %w", req.DefaultKernelImage, err)
	}
	if _, err := s.client.Fetch(pullCtx, req.DefaultKernelImage, containerd.WithResolver(kernelResolver)); err != nil {
		return runtimeapi.PullImageResult{}, fmt.Errorf("fetch default kernel image %s content: %w", req.DefaultKernelImage, err)
	}
	results, err = conchimage.UnpackAllSubImagesWithDefaultSandbox(pullCtx, s.client.Client, req.ImageName, req.DefaultKernelImage)
	if err != nil {
		return runtimeapi.PullImageResult{}, fmt.Errorf("unpack pulled image with default kernel image %s: %w", req.DefaultKernelImage, err)
	}
	return runtimeapi.PullImageResult{Refs: results}, nil
}

func (s *Service) PrepareRootfsSource(ctx context.Context, req conchimage.PrepareRootfsSourceOptions) (conchimage.PrepareRootfsSourceResult, error) {
	if s == nil || s.client == nil {
		return conchimage.PrepareRootfsSourceResult{}, fmt.Errorf("image service has no containerd client")
	}
	if req.Source == "" {
		return conchimage.PrepareRootfsSourceResult{}, fmt.Errorf("%w: source is required", conchimage.ErrInvalidRequest)
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
			return conchimage.PrepareRootfsSourceResult{}, fmt.Errorf("lookup rootfs source image %s: %w", req.Source, err)
		}
		resolver := docker.NewResolver(docker.ResolverOptions{
			PlainHTTP: req.PlainHTTP,
			Credentials: func(string) (string, string, error) {
				return req.Username, req.Password, nil
			},
		})
		pulled, err := s.client.Pull(sourceCtx, req.Source, containerd.WithResolver(resolver))
		if err != nil {
			return conchimage.PrepareRootfsSourceResult{}, fmt.Errorf("pull rootfs source image %s: %w", req.Source, err)
		}
		img = pulled
	}

	imageName := img.Name()
	if req.TargetImage != "" && req.TargetImage != imageName {
		alias := images.Image{Name: req.TargetImage, Target: img.Target()}
		if _, err := s.client.ImageService().Create(sourceCtx, alias); err != nil {
			if errdefs.IsAlreadyExists(err) {
				if _, updateErr := s.client.ImageService().Update(sourceCtx, alias, "target"); updateErr != nil {
					return conchimage.PrepareRootfsSourceResult{}, fmt.Errorf("update rootfs source image alias %s: %w", req.TargetImage, updateErr)
				}
			} else {
				return conchimage.PrepareRootfsSourceResult{}, fmt.Errorf("create rootfs source image alias %s: %w", req.TargetImage, err)
			}
		}
		imageName = req.TargetImage
	}

	return conchimage.PrepareRootfsSourceResult{
		ImageName:      imageName,
		ManifestDigest: img.Target().Digest.String(),
	}, nil
}

func (s *Service) Push(ctx context.Context, req runtimeapi.PushImageOptions) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("image service has no containerd client")
	}
	if req.LocalImage == "" {
		return fmt.Errorf("%w: local_image is required", conchimage.ErrInvalidRequest)
	}
	if req.RemoteImage == "" {
		return fmt.Errorf("%w: remote_image is required", conchimage.ErrInvalidRequest)
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

func (s *Service) List(ctx context.Context, req runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error) {
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
	out := make([]runtimeapi.ImageRecord, 0, len(items))
	for _, item := range items {
		kind := imageKindFromLabels(item.Labels)
		if kind == "" {
			kind = s.classifyImageKind(listCtx, item)
		}
		out = append(out, runtimeapi.ImageRecord{
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

func (s *Service) Remove(ctx context.Context, req runtimeapi.RemoveImageOptions) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("image service has no containerd client")
	}
	if req.ImageName == "" {
		return fmt.Errorf("%w: image_name is required", conchimage.ErrInvalidRequest)
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

func (s *Service) Unpack(ctx context.Context, req runtimeapi.UnpackImageOptions) (map[string]string, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("image service has no containerd client")
	}
	if req.ImageName == "" {
		return nil, fmt.Errorf("%w: image_name is required", conchimage.ErrInvalidRequest)
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

func (s *Service) ImportArchive(ctx context.Context, archive io.Reader, req runtimeapi.ImportImageArchiveOptions) (runtimeapi.ImportImageArchiveResult, error) {
	if s == nil || s.client == nil {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("image service has no containerd client")
	}
	if archive == nil {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("%w: archive is required", conchimage.ErrInvalidRequest)
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
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("containerd import failed: %w", err)
	}
	if len(importedImages) == 0 {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("no images were imported")
	}

	snapshotter := s.client.SnapshotService("erofs")
	finalSnapshotKey, finalImageName, err := selectImportedSnapshot(
		reorderImportedImages(importedImages, req.ImportedTag),
		func(imgInfo images.Image) (map[string]string, bool, error) {
			if err := conchimage.ValidateConchImageIndex(importCtx, s.client.Client, imgInfo.Name); err != nil {
				return nil, false, nil
			}
			snapshotMap, err := conchimage.UnpackAllSubImages(importCtx, s.client.Client, imgInfo.Name)
			if err != nil {
				return nil, true, fmt.Errorf("failed to unpack Conch image index %s: %w", imgInfo.Name, err)
			}
			return snapshotMap, true, nil
		},
		func(imgInfo images.Image) (string, error) {
			img := containerd.NewImage(s.client.Client, imgInfo)
			if err := img.Unpack(importCtx, "erofs"); err != nil {
				return "", fmt.Errorf("failed to unpack image %s: %w", imgInfo.Name, err)
			}

			diffIDs, err := img.RootFS(importCtx)
			if err != nil {
				return "", fmt.Errorf("failed to get rootfs: %w", err)
			}
			snapshotKey := identity.ChainID(diffIDs).String()
			if _, err := snapshotter.Stat(importCtx, snapshotKey); err != nil {
				return "", nil
			}
			return snapshotKey, nil
		},
	)
	if err != nil {
		return runtimeapi.ImportImageArchiveResult{}, err
	}

	if finalSnapshotKey == "" {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("no snapshot key generated")
	}
	return runtimeapi.ImportImageArchiveResult{
		SnapshotKey: finalSnapshotKey,
		ImageName:   finalImageName,
	}, nil
}

func selectImportedSnapshot(importedImages []images.Image, unpackConchIndex func(images.Image) (map[string]string, bool, error), unpackRegularImage func(images.Image) (string, error)) (string, string, error) {
	for _, imgInfo := range importedImages {
		if imgInfo.Target.MediaType == ocispec.MediaTypeImageIndex {
			snapshotMap, isConchIndex, err := unpackConchIndex(imgInfo)
			if err != nil {
				return "", "", err
			}
			if isConchIndex {
				return snapshotMap[conchimage.KindRootfs], imgInfo.Name, nil
			}
		}
		snapshotKey, err := unpackRegularImage(imgInfo)
		if err != nil {
			return "", "", err
		}
		if snapshotKey == "" {
			continue
		}
		return snapshotKey, imgInfo.Name, nil
	}
	return "", "", nil
}

func (s *Service) ExportArchive(ctx context.Context, w io.Writer, req runtimeapi.ExportImageArchiveOptions) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("image service has no containerd client")
	}
	if w == nil {
		return fmt.Errorf("%w: archive writer is required", conchimage.ErrInvalidRequest)
	}
	if req.ImageName == "" {
		return fmt.Errorf("%w: image_name is required", conchimage.ErrInvalidRequest)
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

func (s *Service) PublishBootImage(ctx context.Context, req conchimage.PublishBootImageOptions) (conchimage.PublishBootImageResult, error) {
	if s == nil || s.client == nil {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("image service has no containerd client")
	}
	if req.RootfsImageName == "" {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("%w: rootfs_image_name is required", conchimage.ErrInvalidRequest)
	}
	if req.KernelPath == "" {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("%w: kernel_path is required", conchimage.ErrInvalidRequest)
	}
	if req.InitrdPath == "" {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("%w: initrd_path is required", conchimage.ErrInvalidRequest)
	}
	if req.BootIndexTag == "" {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("%w: boot_index_tag is required", conchimage.ErrInvalidRequest)
	}

	ns := req.Namespace
	if ns == "" {
		ns = s.client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	namespaceCtx := namespaces.WithNamespace(ctx, ns)
	publishCtx, done, err := s.client.WithLease(namespaceCtx)
	if err != nil {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("create content lease: %w", err)
	}
	defer done(publishCtx)

	rootfsImage, err := s.client.ImageService().Get(publishCtx, req.RootfsImageName)
	if err != nil {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("lookup rootfs image %s: %w", req.RootfsImageName, err)
	}
	indexDesc, err := conchimage.BuildBootIndexInContent(publishCtx, s.client.ContentStore(), conchimage.BootIndexContentOptions{
		RootfsDescriptor: rootfsImage.Target,
		KernelPath:       req.KernelPath,
		InitrdPath:       req.InitrdPath,
		Tag:              req.BootIndexTag,
	})
	if err != nil {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("build boot index content: %w", err)
	}

	labelHandler := images.SetChildrenLabels(s.client.ContentStore(), images.ChildrenHandler(s.client.ContentStore()))
	if err := images.WalkNotEmpty(publishCtx, labelHandler, indexDesc); err != nil {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("label boot index content: %w", err)
	}
	imageRecord := images.Image{Name: req.BootIndexTag, Target: indexDesc}
	if _, err := s.client.ImageService().Update(publishCtx, imageRecord, "target"); err != nil {
		if !errdefs.IsNotFound(err) {
			return conchimage.PublishBootImageResult{}, fmt.Errorf("update boot image record %s: %w", req.BootIndexTag, err)
		}
		if _, err := s.client.ImageService().Create(publishCtx, imageRecord); err != nil {
			return conchimage.PublishBootImageResult{}, fmt.Errorf("create boot image record %s: %w", req.BootIndexTag, err)
		}
	}

	snapshotMap, err := conchimage.UnpackAllSubImages(publishCtx, s.client.Client, req.BootIndexTag)
	if err != nil {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("unpack boot image %s: %w", req.BootIndexTag, err)
	}
	snapshotKey := snapshotMap[conchimage.KindRootfs]
	if snapshotKey == "" {
		return conchimage.PublishBootImageResult{}, fmt.Errorf("boot image %s unpack returned empty rootfs snapshot key", req.BootIndexTag)
	}
	return conchimage.PublishBootImageResult{
		BootIndexDigest: indexDesc.Digest.String(),
		SnapshotKey:     snapshotKey,
		ImageName:       req.BootIndexTag,
	}, nil
}

func (s *Service) ConvertRootfsToErofs(ctx context.Context, req erofsconvert.ConvertRootfsRequest) (erofsconvert.ConvertRootfsResult, error) {
	if s == nil || s.client == nil {
		return erofsconvert.ConvertRootfsResult{}, fmt.Errorf("image service has no containerd client")
	}
	if s.converter == nil {
		return erofsconvert.ConvertRootfsResult{}, fmt.Errorf("rootfs erofs converter is not configured")
	}

	if req.Namespace == "" {
		req.Namespace = s.client.DefaultNamespace()
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	normalized, err := erofsconvert.NormalizeRequest(req)
	if err != nil {
		return erofsconvert.ConvertRootfsResult{}, fmt.Errorf("%w: %v", conchimage.ErrInvalidRequest, err)
	}
	result, err := s.converter.Convert(ctx, normalized)
	if err != nil {
		return erofsconvert.ConvertRootfsResult{}, fmt.Errorf("convert rootfs to erofs: %w", err)
	}
	convertCtx := namespaces.WithNamespace(ctx, normalized.Namespace)
	imgInfo, err := s.client.ImageService().Get(convertCtx, result.ImageName)
	if err != nil {
		return erofsconvert.ConvertRootfsResult{}, fmt.Errorf("lookup converted image: %w", err)
	}
	if err := containerd.NewImage(s.client.Client, imgInfo).Unpack(convertCtx, "erofs"); err != nil {
		return erofsconvert.ConvertRootfsResult{}, fmt.Errorf("unpack converted rootfs with erofs snapshotter: %w", err)
	}
	snapshotKey, err := conchimage.GetSnapshotID(ctx, s.client, normalized.Namespace, result.ImageName)
	if err != nil {
		return erofsconvert.ConvertRootfsResult{}, fmt.Errorf("resolve converted rootfs snapshot key: %w", err)
	}
	if _, err := s.client.SnapshotService("erofs").Update(convertCtx, snapshots.Info{
		Name: snapshotKey,
		Labels: map[string]string{
			common.SnapshotLabelRootfsImage:    result.ImageName,
			common.SnapshotLabelRootfsManifest: result.ManifestDigest,
		},
	}, "labels."+common.SnapshotLabelRootfsImage, "labels."+common.SnapshotLabelRootfsManifest); err != nil {
		return erofsconvert.ConvertRootfsResult{}, fmt.Errorf("label converted rootfs snapshot: %w", err)
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
