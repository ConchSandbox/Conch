package cri

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

const (
	runtimeName       = "conch"
	runtimeVersion    = "0.1.0"
	runtimeAPIVersion = "v1"
)

type Config struct {
	Socket             string
	DefaultKernelImage string
	Snapshot           SnapshotConfig
}

// SnapshotConfig configures the CheckpointContainer push target and registry auth.
type SnapshotConfig struct {
	// DefaultRepo fallback; snapshot is pushed to "<DefaultRepo>/<pod-name>:<timestamp>".
	DefaultRepo string
	// Push controls whether the checkpoint image is pushed to a registry.
	// Default false = local-only (no push; restore pinned to the same node).
	Push      bool
	PlainHTTP bool
	Username  string
	Password  string
}

type Runtime interface {
	CreateSandbox(context.Context, runtimeapi.SandboxCreateOptions) (runtimeapi.SandboxCreateResult, error)
	StopSandbox(context.Context, string, string) error
	RemoveSandbox(context.Context, string, string) error
	CreateContainer(context.Context, runtimeapi.ContainerCreateOptions) (runtimeapi.ContainerCreateResult, error)
	SetContainerState(context.Context, string, string) error
	PullImage(context.Context, runtimeapi.PullImageOptions) (runtimeapi.PullImageResult, error)
	ListImages(context.Context, runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error)
	RemoveImage(context.Context, runtimeapi.RemoveImageOptions) error
}

type Server struct {
	cfg          Config
	runtime      Runtime
	store        state.Store
	checkpointer Checkpointer
	grpc         *grpc.Server
	ln           net.Listener
}

// New creates a CRI server. checkpointer may be nil, in which case
// CheckpointContainer returns Unimplemented.
func New(cfg Config, runtime Runtime, store state.Store, checkpointer Checkpointer) *Server {
	return &Server{cfg: cfg, runtime: runtime, store: store, checkpointer: checkpointer}
}

func (s *Server) Start() error {
	if s == nil {
		return fmt.Errorf("cri server is nil")
	}
	if s.cfg.Socket == "" {
		return fmt.Errorf("cri socket is required")
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.Socket), 0o755); err != nil {
		return fmt.Errorf("create cri socket dir: %w", err)
	}
	if err := removeStaleUnixSocket(s.cfg.Socket); err != nil {
		return err
	}
	ln, err := net.Listen("unix", s.cfg.Socket)
	if err != nil {
		return fmt.Errorf("listen cri socket %s: %w", s.cfg.Socket, err)
	}
	if err := os.Chmod(s.cfg.Socket, 0o660); err != nil {
		_ = ln.Close()
		_ = os.Remove(s.cfg.Socket)
		return fmt.Errorf("chmod cri socket: %w", err)
	}

	grpcServer := grpc.NewServer()
	svc := &service{cfg: s.cfg, runtime: s.runtime, store: s.store, checkpointer: s.checkpointer}
	runtimev1.RegisterRuntimeServiceServer(grpcServer, svc)
	runtimev1.RegisterImageServiceServer(grpcServer, svc)

	s.grpc = grpcServer
	s.ln = ln
	go func() {
		_ = grpcServer.Serve(ln)
	}()
	return nil
}

func (s *Server) Stop() {
	if s == nil {
		return
	}
	if s.grpc != nil {
		s.grpc.GracefulStop()
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
	if s.cfg.Socket != "" {
		_ = os.Remove(s.cfg.Socket)
	}
}

func removeStaleUnixSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat cri socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("cri socket path exists and is not a unix socket: %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale cri socket: %w", err)
	}
	return nil
}

type service struct {
	runtimev1.UnimplementedRuntimeServiceServer
	runtimev1.UnimplementedImageServiceServer

	cfg          Config
	runtime      Runtime
	store        state.Store
	checkpointer Checkpointer
}

func (s *service) Version(context.Context, *runtimev1.VersionRequest) (*runtimev1.VersionResponse, error) {
	return &runtimev1.VersionResponse{
		Version:           runtimeAPIVersion,
		RuntimeName:       runtimeName,
		RuntimeVersion:    runtimeVersion,
		RuntimeApiVersion: runtimeAPIVersion,
	}, nil
}

func (s *service) Status(_ context.Context, req *runtimev1.StatusRequest) (*runtimev1.StatusResponse, error) {
	resp := &runtimev1.StatusResponse{
		Status: &runtimev1.RuntimeStatus{
			Conditions: []*runtimev1.RuntimeCondition{
				{Type: "RuntimeReady", Status: true},
				{Type: "NetworkReady", Status: true},
			},
		},
		Features: &runtimev1.RuntimeFeatures{},
	}
	if req != nil && req.Verbose {
		resp.Info = map[string]string{
			"runtime": `"conch"`,
		}
	}
	return resp, nil
}

func (s *service) RuntimeConfig(context.Context, *runtimev1.RuntimeConfigRequest) (*runtimev1.RuntimeConfigResponse, error) {
	return &runtimev1.RuntimeConfigResponse{
		Linux: &runtimev1.LinuxRuntimeConfiguration{
			CgroupDriver: runtimev1.CgroupDriver_SYSTEMD,
		},
	}, nil
}
