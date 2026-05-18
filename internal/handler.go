package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	imageSvc "github.com/openeuler/Conch/internal/conchservices/image"
	snapshotSvc "github.com/openeuler/Conch/internal/conchservices/snapshot"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/containerdhost"
	"github.com/openeuler/Conch/internal/daemon"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/snapshot/common"
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

var (
	buildKernelArchiveFromFiles = conchimage.BuildKernelArchiveFromFiles
	buildBootIndexArchive       = conchimage.BuildBootIndexArchive
)

type sandboxManager interface {
	Create(req sandbox.SandboxCreateRequest) (string, error)
	Delete(req sandbox.SandboxDeleteRequest) error
	Pause(req sandbox.SandboxPauseRequest) (string, error)
}

type imageService interface {
	Pull(context.Context, imageSvc.PullRequest) (map[string]string, error)
	Push(context.Context, imageSvc.PushRequest) error
	Unpack(context.Context, imageSvc.UnpackRequest) (map[string]string, error)
	ImportArchive(context.Context, io.Reader, imageSvc.ImportArchiveRequest) (imageSvc.ImportArchiveResponse, error)
	ExportArchive(context.Context, io.Writer, imageSvc.ExportArchiveRequest) error
	PrepareRootfsSource(context.Context, imageSvc.PrepareRootfsSourceRequest) (imageSvc.PrepareRootfsSourceResponse, error)
	ConvertRootfsToErofs(context.Context, imageSvc.ConvertRootfsToErofsRequest) (imageSvc.ConvertRootfsToErofsResponse, error)
}

type snapshotService interface {
	LinkVM(context.Context, snapshotSvc.LinkVMRequest) error
	Info(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Meta, error)
	Chain(context.Context, snapshotSvc.InfoRequest) (snapshotSvc.Chain, error)
}

type convertImageRequest struct {
	Source       string `json:"source"`
	Namespace    string `json:"namespace,omitempty"`
	BootIndexTag string `json:"boot_index_tag"`
	PlainHTTP    bool   `json:"plain_http,omitempty"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	Snapshot     bool   `json:"snapshot,omitempty"`
}

type convertImageResponse struct {
	BootIndexDigest string `json:"boot_index_digest"`
	BootIndexTag    string `json:"boot_index_tag"`
	RootfsImageRef  string `json:"rootfs_image_ref,omitempty"`
	KernelImageRef  string `json:"kernel_image_ref,omitempty"`
	SourceImageRef  string `json:"source_image_ref,omitempty"`
}

type snapshotExportRequest struct {
	Namespace        string `json:"namespace,omitempty"`
	BootIndexTag     string `json:"boot_index_tag"`
	RootfsSnapshotID string `json:"snapshot_id,omitempty"`
	SandboxID        string `json:"sandbox_id,omitempty"`
}

type snapshotExportResponse struct {
	BootIndexDigest string `json:"boot_index_digest"`
	BootIndexTag    string `json:"boot_index_tag"`
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
	s.router.HandleFunc("/api/image/push", s.handlePushImage)
	s.router.HandleFunc("/api/image/unpack", s.handleUnpackImage)
	s.router.HandleFunc("/api/image/convert", s.handleConvertImage)
	s.router.HandleFunc("/api/snapshot/export", s.handleSnapshotExport)
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

func (s *Server) handlePushImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling push image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.imageService == nil {
		http.Error(w, "Image service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req imageSvc.PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("Invalid request body", ulog.F("error", err))
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.imageService.Push(r.Context(), req); err != nil {
		logger.Error("Failed to push image",
			ulog.F("local_image", req.LocalImage),
			ulog.F("remote_image", req.RemoteImage),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to push image", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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

func (s *Server) handleConvertImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling convert image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.imageService == nil || s.snapshotService == nil {
		http.Error(w, "Image or snapshot service unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "Invalid multipart body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req convertImageRequest
	if err := json.Unmarshal([]byte(r.FormValue("metadata")), &req); err != nil {
		http.Error(w, "Invalid metadata: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Snapshot && s.sandboxManager == nil {
		http.Error(w, "Sandbox service unavailable", http.StatusServiceUnavailable)
		return
	}

	tmpDir, err := os.MkdirTemp("", "conch-convert-api-*")
	if err != nil {
		http.Error(w, "Failed to create temp workspace: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)
	kernelPath, err := saveMultipartFile(r, "kernel", tmpDir, "kernel")
	if err != nil {
		http.Error(w, "Invalid kernel file: "+err.Error(), http.StatusBadRequest)
		return
	}
	initrdPath, err := saveMultipartFile(r, "initrd", tmpDir, "initrd")
	if err != nil {
		http.Error(w, "Invalid initrd file: "+err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := s.convertImage(r.Context(), req, kernelPath, initrdPath, tmpDir)
	if err != nil {
		logger.Error("Failed to convert image",
			ulog.F("source", req.Source),
			ulog.F("tag", req.BootIndexTag),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to convert image", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleSnapshotExport(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling snapshot export request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.imageService == nil || s.snapshotService == nil {
		http.Error(w, "Image or snapshot service unavailable", http.StatusServiceUnavailable)
		return
	}
	var req snapshotExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SandboxID != "" && s.sandboxManager == nil {
		http.Error(w, "Sandbox service unavailable", http.StatusServiceUnavailable)
		return
	}

	resp, err := s.exportSnapshotImage(r.Context(), req)
	if err != nil {
		logger.Error("Failed to export snapshot image",
			ulog.F("snapshot_id", req.RootfsSnapshotID),
			ulog.F("sandbox_id", req.SandboxID),
			ulog.F("tag", req.BootIndexTag),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to export snapshot image", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) convertImage(ctx context.Context, req convertImageRequest, kernelPath, initrdPath, tmpDir string) (convertImageResponse, error) {
	if strings.TrimSpace(req.Source) == "" {
		return convertImageResponse{}, fmt.Errorf("%w: source is required", imageSvc.ErrInvalidRequest)
	}
	if strings.TrimSpace(req.BootIndexTag) == "" {
		return convertImageResponse{}, fmt.Errorf("%w: boot_index_tag is required", imageSvc.ErrInvalidRequest)
	}
	namespace := s.resolveNamespace(req.Namespace)

	prepared, err := s.imageService.PrepareRootfsSource(ctx, imageSvc.PrepareRootfsSourceRequest{
		Source:    req.Source,
		Namespace: namespace,
		PlainHTTP: req.PlainHTTP,
		Username:  req.Username,
		Password:  req.Password,
	})
	if err != nil {
		return convertImageResponse{}, fmt.Errorf("prepare rootfs source: %w", err)
	}

	convertTarget := fmt.Sprintf("conch-erofs-rootfs:convert-%d", time.Now().UnixNano())
	convertResp, err := s.imageService.ConvertRootfsToErofs(ctx, imageSvc.ConvertRootfsToErofsRequest{
		Namespace:   namespace,
		SourceImage: prepared.ImageName,
		TargetImage: convertTarget,
		MkfsOptions: []string{erofsconvert.DefaultMkfsOption},
		AlignBytes:  erofsconvert.DefaultAlignBytes,
	})
	if err != nil {
		return convertImageResponse{}, fmt.Errorf("convert rootfs to EROFS: %w", err)
	}
	rootfsSnapshotID := strings.TrimSpace(convertResp.SnapshotKey)
	if rootfsSnapshotID == "" {
		return convertImageResponse{}, fmt.Errorf("convert rootfs to EROFS returned empty snapshot key")
	}

	kernelImageRef := fmt.Sprintf("conch-kernel:convert-%d", time.Now().UnixNano())
	kernelArchive := filepath.Join(tmpDir, "kernel.oci.tar")
	if _, err := buildKernelArchiveFromFiles(ctx, kernelPath, initrdPath, kernelImageRef, kernelArchive); err != nil {
		return convertImageResponse{}, fmt.Errorf("build native kernel component archive: %w", err)
	}
	vmImport, err := s.importImageArchiveFromPath(ctx, kernelArchive, namespace, kernelImageRef)
	if err != nil {
		return convertImageResponse{}, fmt.Errorf("import kernel component image %s: %w", kernelImageRef, err)
	}
	if err := s.snapshotService.LinkVM(ctx, snapshotSvc.LinkVMRequest{
		RootfsSnapshotID: rootfsSnapshotID,
		VMSnapshotID:     vmImport.SnapshotKey,
		Namespace:        namespace,
	}); err != nil {
		return convertImageResponse{}, fmt.Errorf("link rootfs snapshot to sandbox snapshot: %w", err)
	}

	if req.Snapshot {
		sandboxID := fmt.Sprintf("conch-snap-%d", time.Now().UnixNano())
		if _, err := s.sandboxManager.Create(sandbox.SandboxCreateRequest{
			Namespace: namespace,
			ImageName: convertResp.ImageName,
			VmmName:   "cloud-hypervisor",
			SandboxId: sandboxID,
			VcpuNum:   1,
			RamMB:     256,
		}); err != nil {
			return convertImageResponse{}, fmt.Errorf("create sandbox for snapshot conversion: %w", err)
		}
		rootfsSnapshotID, err = s.sandboxManager.Pause(sandbox.SandboxPauseRequest{
			Namespace: namespace,
			SandboxId: sandboxID,
		})
		if err != nil {
			return convertImageResponse{}, fmt.Errorf("pause sandbox for snapshot conversion: %w", err)
		}
		snapshotResp, err := s.exportSnapshotImage(ctx, snapshotExportRequest{
			Namespace:        namespace,
			BootIndexTag:     req.BootIndexTag,
			RootfsSnapshotID: rootfsSnapshotID,
		})
		if err != nil {
			return convertImageResponse{}, err
		}
		return convertImageResponse{
			BootIndexDigest: snapshotResp.BootIndexDigest,
			BootIndexTag:    snapshotResp.BootIndexTag,
			RootfsImageRef:  convertResp.ImageName,
			KernelImageRef:  kernelImageRef,
			SourceImageRef:  prepared.ImageName,
		}, nil
	}

	bootDigest, err := s.publishBootIndex(ctx, convertResp.ImageName, kernelArchive, req.BootIndexTag, namespace, tmpDir)
	if err != nil {
		return convertImageResponse{}, err
	}
	return convertImageResponse{
		BootIndexDigest: bootDigest,
		BootIndexTag:    req.BootIndexTag,
		RootfsImageRef:  convertResp.ImageName,
		KernelImageRef:  kernelImageRef,
		SourceImageRef:  prepared.ImageName,
	}, nil
}

func (s *Server) publishBootIndex(ctx context.Context, rootfsImageName, kernelArchive, bootIndexTag, namespace, tmpDir string) (string, error) {
	rootfsArchive := filepath.Join(tmpDir, "rootfs.oci.tar")
	if err := s.exportImageArchiveToPath(ctx, rootfsArchive, namespace, rootfsImageName); err != nil {
		return "", fmt.Errorf("export native rootfs image: %w", err)
	}

	bootArchive := filepath.Join(tmpDir, "boot-index.oci.tar")
	bootDigest, err := buildBootIndexArchive(ctx, conchimage.BootIndexOptions{
		RootfsArchivePath:  rootfsArchive,
		SandboxArchivePath: kernelArchive,
		Tag:                bootIndexTag,
		ArchivePath:        bootArchive,
	})
	if err != nil {
		return "", fmt.Errorf("build native boot index archive: %w", err)
	}
	if _, err := s.importImageArchiveFromPath(ctx, bootArchive, namespace, bootIndexTag); err != nil {
		return "", fmt.Errorf("import native boot index archive: %w", err)
	}
	return bootDigest.String(), nil
}

func (s *Server) exportSnapshotImage(ctx context.Context, req snapshotExportRequest) (snapshotExportResponse, error) {
	if strings.TrimSpace(req.BootIndexTag) == "" {
		return snapshotExportResponse{}, fmt.Errorf("%w: boot_index_tag is required", imageSvc.ErrInvalidRequest)
	}
	if (strings.TrimSpace(req.RootfsSnapshotID) == "") == (strings.TrimSpace(req.SandboxID) == "") {
		return snapshotExportResponse{}, fmt.Errorf("%w: exactly one of snapshot_id or sandbox_id is required", imageSvc.ErrInvalidRequest)
	}
	namespace := s.resolveNamespace(req.Namespace)
	rootfsSnapshotID := strings.TrimSpace(req.RootfsSnapshotID)
	if rootfsSnapshotID == "" {
		snapshotID, err := s.sandboxManager.Pause(sandbox.SandboxPauseRequest{
			Namespace: namespace,
			SandboxId: req.SandboxID,
		})
		if err != nil {
			return snapshotExportResponse{}, fmt.Errorf("pause sandbox: %w", err)
		}
		rootfsSnapshotID = snapshotID
	}

	rootfsInfo, err := s.snapshotService.Info(ctx, snapshotSvc.InfoRequest{Key: rootfsSnapshotID, Namespace: namespace})
	if err != nil {
		return snapshotExportResponse{}, fmt.Errorf("resolve rootfs snapshot metadata: %w", err)
	}
	memName, vmName, err := resolveSnapshotComponentIDs(rootfsInfo)
	if err != nil {
		return snapshotExportResponse{}, err
	}

	memChain, err := s.snapshotService.Chain(ctx, snapshotSvc.InfoRequest{Key: memName, Namespace: namespace})
	if err != nil {
		return snapshotExportResponse{}, fmt.Errorf("resolve mem snapshot chain: %w", err)
	}
	memChainPaths, err := validateSnapshotChainPaths(memChain.ChainPaths)
	if err != nil {
		return snapshotExportResponse{}, fmt.Errorf("resolve mem snapshot chain: %w", err)
	}
	vmChain, err := s.snapshotService.Chain(ctx, snapshotSvc.InfoRequest{Key: vmName, Namespace: namespace})
	if err != nil {
		return snapshotExportResponse{}, fmt.Errorf("resolve sandbox snapshot chain: %w", err)
	}
	vmChainPaths, err := validateSnapshotChainPaths(vmChain.ChainPaths)
	if err != nil {
		return snapshotExportResponse{}, fmt.Errorf("resolve sandbox snapshot chain: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "conch-snapshot-export-api-*")
	if err != nil {
		return snapshotExportResponse{}, fmt.Errorf("create snapshot export temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	rootfsArchive, err := s.exportNativeRootfsArchive(ctx, tmpDir, &rootfsInfo, namespace)
	if err != nil {
		return snapshotExportResponse{}, err
	}

	bootArchive := filepath.Join(tmpDir, "boot-index.oci.tar")
	bootDigest, err := buildBootIndexArchive(ctx, conchimage.BootIndexOptions{
		RootfsArchivePath: rootfsArchive,
		MemChainPaths:     memChainPaths,
		SandboxChainPaths: vmChainPaths,
		Tag:               req.BootIndexTag,
		ArchivePath:       bootArchive,
	})
	if err != nil {
		return snapshotExportResponse{}, fmt.Errorf("build native boot index archive: %w", err)
	}
	if _, err := s.importImageArchiveFromPath(ctx, bootArchive, namespace, req.BootIndexTag); err != nil {
		return snapshotExportResponse{}, fmt.Errorf("import native boot index archive: %w", err)
	}
	return snapshotExportResponse{BootIndexDigest: bootDigest.String(), BootIndexTag: req.BootIndexTag}, nil
}

func (s *Server) exportNativeRootfsArchive(ctx context.Context, tmpDir string, rootfsInfo *snapshotSvc.Meta, namespace string) (string, error) {
	if rootfsInfo == nil {
		return "", fmt.Errorf("rootfs snapshot metadata is required")
	}
	rootfsImage := strings.TrimSpace(rootfsInfo.Labels[common.SnapshotLabelRootfsImage])
	if rootfsImage == "" {
		return "", fmt.Errorf("rootfs snapshot %s does not record native EROFS image label %q", rootfsInfo.Key, common.SnapshotLabelRootfsImage)
	}
	archivePath := filepath.Join(tmpDir, "rootfs.oci.tar")
	if err := s.exportImageArchiveToPath(ctx, archivePath, namespace, rootfsImage); err != nil {
		return "", fmt.Errorf("export native rootfs image: %w", err)
	}
	return archivePath, nil
}

func (s *Server) importImageArchiveFromPath(ctx context.Context, archivePath, namespace, importedTag string) (imageSvc.ImportArchiveResponse, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return imageSvc.ImportArchiveResponse{}, fmt.Errorf("open archive: %w", err)
	}
	defer file.Close()
	return s.imageService.ImportArchive(ctx, file, imageSvc.ImportArchiveRequest{
		Namespace:   namespace,
		ImportedTag: importedTag,
	})
}

func (s *Server) exportImageArchiveToPath(ctx context.Context, archivePath, namespace, imageName string) error {
	file, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer file.Close()
	return s.imageService.ExportArchive(ctx, file, imageSvc.ExportArchiveRequest{
		Namespace: namespace,
		ImageName: imageName,
	})
}

func (s *Server) resolveNamespace(namespace string) string {
	if ns := strings.TrimSpace(namespace); ns != "" {
		return ns
	}
	if s.daemonClient != nil {
		if ns := strings.TrimSpace(s.daemonClient.DefaultNamespace()); ns != "" {
			return ns
		}
	}
	return "default"
}

func saveMultipartFile(r *http.Request, field, dir, name string) (string, error) {
	file, _, err := r.FormFile(field)
	if err != nil {
		return "", err
	}
	defer file.Close()
	path := filepath.Join(dir, name)
	if err := writeMultipartFile(path, file); err != nil {
		return "", err
	}
	return path, nil
}

func writeMultipartFile(path string, file multipart.File) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		return err
	}
	return nil
}

func validateSnapshotChainPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("snapshot chain is empty")
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("snapshot chain contains empty storage path")
		}
	}
	return paths, nil
}

func resolveSnapshotComponentIDs(rootfsInfo snapshotSvc.Meta) (string, string, error) {
	memName := strings.TrimSpace(rootfsInfo.Labels[common.SnapshotLabelMemSnapshot])
	if memName == "" {
		return "", "", fmt.Errorf("rootfs snapshot %s missing mem snapshot label %q", rootfsInfo.Key, common.SnapshotLabelMemSnapshot)
	}
	vmName := strings.TrimSpace(rootfsInfo.Labels[common.SnapshotLabelVMSnapshot])
	if vmName == "" {
		return "", "", fmt.Errorf("rootfs snapshot %s missing sandbox snapshot label %q", rootfsInfo.Key, common.SnapshotLabelVMSnapshot)
	}
	return memName, vmName, nil
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

// TODO return available snapshot_id
func (s *Server) handleListSnapshot(w http.ResponseWriter, r *http.Request) {}
