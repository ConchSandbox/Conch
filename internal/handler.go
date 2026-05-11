package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	imageSvc "github.com/openeuler/Conch/internal/conchservices/image"
	snapshotSvc "github.com/openeuler/Conch/internal/conchservices/snapshot"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/containerdhost"
	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	shutdownTimeout = 30 * time.Second
)

type Server struct {
	router          *http.ServeMux
	sandboxManager  sandboxManager
	imageService    imageService
	snapshotService snapshotService
	containerdHost  *containerdhost.Host
	daemonClient    *daemon.Client
	httpServer      *http.Server
	listener        net.Listener
	unixSocketPath  string
	cleanupOnce     sync.Once
	defaultKernel   string

	// TODO: need ListCachedBuilds()
}

type sandboxManager interface {
	Create(req sandbox.SandboxCreateRequest) (string, error)
	Delete(req sandbox.SandboxDeleteRequest) error
	Pause(req sandbox.SandboxPauseRequest) (string, error)
}

type imageService interface {
	Pull(context.Context, imageSvc.PullRequest) (map[string]string, error)
	Unpack(context.Context, imageSvc.UnpackRequest) (map[string]string, error)
	ImportArchive(context.Context, io.Reader, imageSvc.ImportArchiveRequest) (imageSvc.ImportArchiveResponse, error)
}

type snapshotService interface {
	LinkVM(context.Context, snapshotSvc.LinkVMRequest) error
	Info(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Meta, error)
	Chain(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Chain, error)
}

func handleSignals(ctx context.Context, cancel context.CancelFunc, s *Server) {
	go func() {
		var sig os.Signal
		var handledSignals = []os.Signal{
			unix.SIGTERM,
			unix.SIGINT,
		}

		// Do not print message when dealing with SIGPIPE, which may cause
		// nested signals and consume lots of cpu bandwidth.
		signal.Ignore(unix.SIGPIPE)

		signalChannel := make(chan os.Signal, 1)
		signal.Notify(signalChannel, handledSignals...)

		for {
			select {
			case <-ctx.Done():
				ulog.Warn("Context done",
					ulog.F("error", ctx.Err()),
				)
			case sig = <-signalChannel:
				ulog.Info("Interrupted by signal, process exiting",
					ulog.F("signal", sig),
				)
				cancel()
				s.Cleanup()

				return
			}
		}
	}()
	return
}

func NewServer(cfg *config.Config) (*Server, error) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		router: http.NewServeMux(),
	}
	s.routes()

	logger := ulog.GetLogger()

	host, err := containerdhost.Start(ctx, containerdhost.Config{
		RootDir:          cfg.Containerd.RootDir,
		StateDir:         cfg.Containerd.StateDir,
		DefaultNamespace: cfg.Containerd.DefaultNamespace,
		Snapshot: containerdhost.SnapshotConfig{
			Enabled: true,
			WorkDir: cfg.Server.WorkDir,
		},
		Sandbox: &containerdhost.SandboxConfig{
			PoolSize:           cfg.Network.PoolSize,
			DynamicReservation: cfg.Network.DynamicReservation,
			BridgeCount:        cfg.Network.BridgeCount,
			TapIP:              cfg.Network.TapIP,
			TapMask:            cfg.Network.TapMask,
			VsockSignalRetry:   cfg.Sandbox.VsockSignalRetry,
			VsockSignalTimeout: cfg.Sandbox.VsockSignalTimeout,
			RequestTimeout:     cfg.Sandbox.RequestTimeout,
		},
	})
	if err != nil {
		cancel()
		logger.Error("Failed to init embedded containerd host", ulog.F("error", err))
		return nil, fmt.Errorf("failed to init embedded containerd host: %w", err)
	}
	s.containerdHost = host
	daemonClient := host.Client()
	s.daemonClient = daemonClient
	s.imageService = host.ImageService()
	s.snapshotService = host.SnapshotService()
	s.SetSandboxManager(host.SandboxService())
	s.defaultKernel = cfg.Image.DefaultKernelImage

	handleSignals(ctx, cancel, s)

	logger.Info("Server initialized successfully")
	return s, nil
}

func (s *Server) SetSandboxManager(manager sandboxManager) {
	s.sandboxManager = manager
}

func (s *Server) routes() {
	// sandbox
	s.router.HandleFunc("/api/sandbox/create", s.handleCreateSandbox)
	s.router.HandleFunc("/api/sandbox/delete", s.handleDeleteSandbox)
	s.router.HandleFunc("/api/sandbox/pause", s.handlePauseSandbox)
	s.router.HandleFunc("/api/snapshot/list", s.handleListSnapshot)
	s.router.HandleFunc("/api/image/pull", s.handlePullImage)
	s.router.HandleFunc("/api/image/unpack", s.handleUnpackImage)
	s.router.HandleFunc("/api/image/import", s.handleImportImage)
	s.router.HandleFunc("/api/snapshot/link-vm", s.handleLinkSnapshotVM)
	s.router.HandleFunc("/api/snapshot/info", s.handleSnapshotInfo)
	s.router.HandleFunc("/api/snapshot/chain", s.handleSnapshotChain)
}

func (s *Server) Start(addr string, unixSocket string) error {
	logger := ulog.GetLogger()
	var (
		err error
		ln  net.Listener
	)

	if unixSocket != "" {
		// If the Unix socket is not empty, then we should use it for server listen port
		// First create the parent directory if needed; this requires permission for the socket path.
		// Then for any existing stale socket it should be removed before start to listen
		if err := os.MkdirAll(filepath.Dir(unixSocket), 0o755); err != nil {
			return fmt.Errorf("failed to create unix socket directory: %w", err)
		}
		if err := os.Remove(unixSocket); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove stale unix socket: %w", err)
		}

		ln, err = net.Listen("unix", unixSocket)
		if err != nil {
			return fmt.Errorf("failed to listen on unix socket %s: %w", unixSocket, err)
		}
		if err := os.Chmod(unixSocket, 0o660); err != nil {
			_ = ln.Close()
			_ = os.Remove(unixSocket)
			return fmt.Errorf("failed to set unix socket permissions: %w", err)
		}

		s.unixSocketPath = unixSocket
		logger.Info("Starting HTTP server", ulog.F("network", "unix"), ulog.F("socket", unixSocket))
	} else {
		// If the Unix socket is empty, then we should use tcp IP for server listen port
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to listen on address %s: %w", addr, err)
		}
		logger.Info("Starting HTTP server", ulog.F("network", "tcp"), ulog.F("address", addr))
	}

	s.listener = ln
	s.httpServer = &http.Server{Handler: s.router}
	err = s.httpServer.Serve(ln)
	if err == http.ErrServerClosed {
		logger.Info("Main server gracefully stopped")
		err = nil
	}
	return err
}

func (s *Server) Cleanup() {
	logger := ulog.GetLogger()

	s.cleanupOnce.Do(func() {
		// stop httpServer
		if s.httpServer != nil {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer shutdownCancel()
			if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
				logger.Error("HTTP server shutdown error", ulog.F("error", err))
			} else {
				logger.Info("HTTP server gracefully stopped")
			}
		}

		if s.unixSocketPath != "" {
			if err := os.Remove(s.unixSocketPath); err != nil && !os.IsNotExist(err) {
				logger.Error("Failed to remove unix socket", ulog.F("socket", s.unixSocketPath), ulog.F("error", err))
			} else {
				logger.Info("Removed unix socket", ulog.F("socket", s.unixSocketPath))
			}
		}

		if s.containerdHost != nil {
			if err := s.containerdHost.Close(); err != nil {
				logger.Error("Containerd cleanup error", ulog.F("error", err))
			}
		} else if s.daemonClient != nil {
			if err := s.daemonClient.Close(); err != nil {
				logger.Error("Containerd cleanup error", ulog.F("error", err))
			}
		}
		logger.Info("Cleanup completed")
	})
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling create sandbox request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req = sandbox.SandboxCreateRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("Invalid request body", ulog.F("error", err))
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	peerIP, err := s.sandboxManager.Create(req)
	if err != nil {
		logger.Error("Failed to create sandbox",
			ulog.F("sandbox_id", req.SandboxId),
			ulog.F("error", err),
		)
		http.Error(w, "Failed to create sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Sandbox created successfully",
		ulog.F("sandbox_id", req.SandboxId),
		ulog.F("peer_ip", peerIP),
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"ip":     peerIP,
	})
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling delete sandbox request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sandbox.SandboxDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("Invalid request body", ulog.F("error", err))
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	err := s.sandboxManager.Delete(req)
	if err != nil {
		logger.Error("Failed to delete sandbox",
			ulog.F("sandbox_id", req.SandboxId),
			ulog.F("error", err),
		)
		http.Error(w, "Failed to delete sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Sandbox deleted successfully", ulog.F("sandbox_id", req.SandboxId))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handlePauseSandbox(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling pause sandbox request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sandbox.SandboxPauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("Invalid request body", ulog.F("error", err))
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	snapshotId, err := s.sandboxManager.Pause(req)
	if err != nil {
		logger.Error("Failed to pause sandbox",
			ulog.F("sandbox_id", req.SandboxId),
			ulog.F("error", err),
		)
		http.Error(w, "Failed to pause sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Sandbox paused successfully",
		ulog.F("sandbox_id", req.SandboxId),
		ulog.F("snapshot_id", snapshotId),
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":     "ok",
		"snapshotId": snapshotId,
	})
}

func (s *Server) handlePullImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling pull image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.imageService == nil {
		http.Error(w, "Image service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req imageSvc.PullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("Invalid request body", ulog.F("error", err))
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.DefaultKernelImage == "" {
		req.DefaultKernelImage = s.defaultKernel
	}

	results, err := s.imageService.Pull(r.Context(), req)
	if err != nil {
		logger.Error("Failed to pull image",
			ulog.F("image_name", req.ImageName),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to pull image", err)
		return
	}

	logger.Info("Image pulled successfully", ulog.F("image_name", req.ImageName))
	writeImageResults(w, results)
}

func (s *Server) handleUnpackImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling unpack image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.imageService == nil {
		http.Error(w, "Image service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req imageSvc.UnpackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("Invalid request body", ulog.F("error", err))
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	results, err := s.imageService.Unpack(r.Context(), req)
	if err != nil {
		logger.Error("Failed to unpack image",
			ulog.F("image_name", req.ImageName),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to unpack image", err)
		return
	}

	logger.Info("Image unpacked successfully", ulog.F("image_name", req.ImageName))
	writeImageResults(w, results)
}

func (s *Server) handleImportImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling import image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.imageService == nil {
		http.Error(w, "Image service unavailable", http.StatusServiceUnavailable)
		return
	}

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "Invalid multipart body: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("archive")
	if err != nil {
		http.Error(w, "Missing archive file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	resp, err := s.imageService.ImportArchive(r.Context(), file, imageSvc.ImportArchiveRequest{
		Namespace:   r.FormValue("namespace"),
		ImportedTag: r.FormValue("imported_tag"),
	})
	if err != nil {
		logger.Error("Failed to import image archive", ulog.F("error", err))
		writeImageError(w, "Failed to import image archive", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeImageResults(w http.ResponseWriter, results map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"results": results,
	})
}

func writeImageError(w http.ResponseWriter, prefix string, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, imageSvc.ErrInvalidRequest) || errors.Is(err, imageSvc.ErrOCIConversionFailed) {
		status = http.StatusBadRequest
	}
	http.Error(w, prefix+": "+err.Error(), status)
}

type linkSnapshotVMRequest struct {
	RootfsSnapshotID string `json:"rootfs_snapshot_id"`
	VMSnapshotID     string `json:"vm_snapshot_id"`
	Namespace        string `json:"namespace,omitempty"`
}

type snapshotInfoRequest struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
}

func (s *Server) handleLinkSnapshotVM(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling link snapshot VM request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req linkSnapshotVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RootfsSnapshotID == "" || req.VMSnapshotID == "" {
		http.Error(w, "rootfs_snapshot_id and vm_snapshot_id are required", http.StatusBadRequest)
		return
	}
	if s.snapshotService == nil {
		http.Error(w, "Snapshot service unavailable", http.StatusServiceUnavailable)
		return
	}

	if err := s.snapshotService.LinkVM(r.Context(), snapshotSvc.LinkVMRequest{
		RootfsSnapshotID: req.RootfsSnapshotID,
		VMSnapshotID:     req.VMSnapshotID,
		Namespace:        req.Namespace,
	}); err != nil {
		logger.Error("Failed to link snapshot VM",
			ulog.F("rootfs_snapshot_id", req.RootfsSnapshotID),
			ulog.F("vm_snapshot_id", req.VMSnapshotID),
			ulog.F("error", err),
		)
		http.Error(w, "Failed to link snapshot VM: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleSnapshotInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req snapshotInfoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if s.snapshotService == nil {
		http.Error(w, "Snapshot service unavailable", http.StatusServiceUnavailable)
		return
	}

	info, err := s.snapshotService.Info(r.Context(), snapshotSvc.InfoRequest{
		Key:       req.Key,
		Namespace: req.Namespace,
	})
	if err != nil {
		http.Error(w, "Failed to get snapshot info: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (s *Server) handleSnapshotChain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req snapshotInfoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if s.snapshotService == nil {
		http.Error(w, "Snapshot service unavailable", http.StatusServiceUnavailable)
		return
	}

	chain, err := s.snapshotService.Chain(r.Context(), snapshotSvc.InfoRequest{
		Key:       req.Key,
		Namespace: req.Namespace,
	})
	if err != nil {
		http.Error(w, "Failed to get snapshot chain: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(chain)
}

// TODO return available snapshot_id
func (s *Server) handleListSnapshot(w http.ResponseWriter, r *http.Request) {}
