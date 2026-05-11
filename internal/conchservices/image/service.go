package image

import (
	"context"
	"errors"
	"fmt"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/openeuler/Conch/internal/conchplugins"
	"github.com/openeuler/Conch/internal/daemon"
	conchimage "github.com/openeuler/Conch/internal/image"
)

var (
	ErrInvalidRequest      = errors.New("invalid image request")
	ErrOCIConversionFailed = errors.New("oci conversion failed")
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

type UnpackRequest struct {
	ImageName string `json:"image_name"`
	Namespace string `json:"namespace,omitempty"`
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
		Type: conchplugins.ServicePluginType,
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
