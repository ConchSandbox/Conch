package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/opencontainers/image-spec/identity"

	"github.com/openeuler/Conch/internal/conchplugins"
	"github.com/openeuler/Conch/internal/daemon"
	conchimage "github.com/openeuler/Conch/internal/image"
)

var (
	ErrInvalidRequest      = errors.New("invalid image request")
	ErrOCIConversionFailed = errors.New("oci conversion failed")
)

const SnapshotLabelVMSnapshot = "conch/snapshotter/vm-snapshot"

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

type Service struct {
	client *daemon.Client
}

func New(client *daemon.Client) *Service {
	return &Service{client: client}
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

	if err := conchimage.ValidateConchImageIndex(pullCtx, s.client.Client, req.ImageName); err != nil {
		results, convErr := conchimage.PullAndUnpackOCIImage(pullCtx, s.client.Client, conchimage.PullOCIImageOptions{
			SourceImage:            req.ImageName,
			DefaultKernelImage:     req.DefaultKernelImage,
			SourcePlainHTTP:        req.PlainHTTP,
			SourceRegistryUsername: req.Username,
			SourceRegistryPassword: req.Password,
			KernelPlainHTTP:        req.KernelPlainHTTP,
			KernelRegistryUsername: req.KernelRegistryUsername,
			KernelRegistryPassword: req.KernelRegistryPassword,
		})
		if convErr != nil {
			return nil, fmt.Errorf("%w: image %s is not a supported Conch image and OCI conversion failed: %v", ErrOCIConversionFailed, req.ImageName, convErr)
		}
		return results, nil
	}

	if _, err := s.client.Fetch(pullCtx, req.ImageName, containerd.WithResolver(resolver)); err != nil {
		return nil, fmt.Errorf("fetch all Conch image content: %w", err)
	}

	results, err := conchimage.UnpackAllSubImages(pullCtx, s.client.Client, req.ImageName)
	if err != nil {
		return nil, fmt.Errorf("unpack pulled image: %w", err)
	}
	return results, nil
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
	importedImages, err := s.client.Import(importCtx, archive)
	if err != nil {
		return ImportArchiveResponse{}, fmt.Errorf("containerd import failed: %w", err)
	}
	if len(importedImages) == 0 {
		return ImportArchiveResponse{}, fmt.Errorf("no images were imported")
	}

	snapshotter := s.client.SnapshotService("overlayfs")
	var finalSnapshotKey string
	var finalImageName string
	for _, imgInfo := range reorderImportedImages(importedImages, req.ImportedTag) {
		img := containerd.NewImage(s.client.Client, imgInfo)
		if err := img.Unpack(importCtx, "overlayfs"); err != nil {
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
	DaemonClient() *daemon.Client
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
