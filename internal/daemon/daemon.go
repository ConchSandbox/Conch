package daemon

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
	"strconv"
	"strings"
	"sync"
	"time"

	remoteerrors "github.com/containerd/containerd/v2/core/remotes/errors"
	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/adapters/containerd/host"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/util"
	"github.com/openeuler/Conch/internal/volume"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	shutdownTimeout = 30 * time.Second
)

type Daemon struct {
	router         *http.ServeMux
	containerdHost *containerdhost.Host
	stateStore     state.Store
	runtimeService *conchruntime.Service
	volumeManager  *volume.Manager
	daemonClient   *containerdclient.Client
	httpServer     *http.Server
	listener       net.Listener
	unixSocketPath string
	cleanupOnce    sync.Once

	// TODO: need ListCachedBuilds()
}

func handleSignals(ctx context.Context, cancel context.CancelFunc, s *Daemon) {
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

func New(cfg *config.Config) (*Daemon, error) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Daemon{
		router: http.NewServeMux(),
	}
	s.routes()

	logger := ulog.GetLogger()

	store, err := state.OpenBolt(cfg.State.Path)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open state store: %w", err)
	}
	s.stateStore = store
	logger.Info("State store initialized", ulog.F("path", cfg.State.Path))
	s.volumeManager, err = volume.NewManager(volume.Config{
		MaxMounts: cfg.Volume.MaxMounts,
		Backend:   cfg.Volume.Backend,
		Virtiofs: volume.VirtiofsConfig{
			Binary:     cfg.Volume.Virtiofs.Binary,
			RuntimeDir: cfg.Volume.Virtiofs.RuntimeDir,
		},
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("init volume manager: %w", err)
	}

	host, err := containerdhost.Start(ctx, containerdhost.Config{
		RootDir:          cfg.Containerd.RootDir,
		StateDir:         cfg.Containerd.StateDir,
		DefaultNamespace: cfg.Containerd.DefaultNamespace,
		TemplateStore:    store,
		Image: containerdhost.ImageConfig{
			DefaultKernelImage:            cfg.Image.DefaultKernelImage,
			DefaultKernelPlainHTTP:        cfg.Image.DefaultKernelPlainHTTP,
			DefaultKernelRegistryUsername: cfg.Image.DefaultKernelRegistryUsername,
			DefaultKernelRegistryPassword: cfg.Image.DefaultKernelRegistryPassword,
		},
		Snapshot: containerdhost.SnapshotConfig{
			WorkDir: cfg.Server.WorkDir,
		},
		Sandbox: &containerdhost.SandboxConfig{
			WarmPoolSize:       cfg.Network.WarmPoolSize,
			TapIP:              cfg.Network.TapIP,
			TapMask:            cfg.Network.TapMask,
			CNI:                cfg.Network.CNI,
			VsockSignalRetry:   cfg.Sandbox.VsockSignalRetry,
			VsockSignalTimeout: cfg.Sandbox.VsockSignalTimeout,
			RequestTimeout:     cfg.Sandbox.RequestTimeout,
			VolumeManager:      s.volumeManager,
		},
	})
	if err != nil {
		cancel()
		_ = store.Close()
		logger.Error("Failed to init embedded containerd host", ulog.F("error", err))
		return nil, fmt.Errorf("failed to init embedded containerd host: %w", err)
	}
	s.containerdHost = host
	daemonClient := host.Client()
	s.daemonClient = daemonClient

	s.runtimeService = conchruntime.New(host.SandboxService(), host.ImageService(), host.ImageService(), store, cfg.Containerd.DefaultNamespace)
	s.runtimeService.Snapshot = host.SnapshotService()
	s.runtimeService.Templates = host.TemplateService()
	s.runtimeService.SetSandboxDefaults(runtimeapi.SandboxDefaults{
		TemplateID: cfg.Sandbox.DefaultTemplateID,
		VMMName:    cfg.Sandbox.DefaultVMMName,
		VCPUNum:    cfg.Sandbox.DefaultVCPUNum,
		VCPUMax:    cfg.Sandbox.DefaultVCPUMax,
		RamMB:      cfg.Sandbox.DefaultRAMMB,
	})

	handleSignals(ctx, cancel, s)

	logger.Info("Server initialized successfully")
	return s, nil
}

func (s *Daemon) routes() {
	// sandbox
	s.router.HandleFunc("GET /api/v1/sandboxes", s.handleListSandbox)
	s.router.HandleFunc("POST /api/v1/sandboxes", s.handleCreateSandbox)
	s.router.HandleFunc("GET /api/v1/sandboxes/{sandboxID}", s.handleGetSandbox)
	s.router.HandleFunc("DELETE /api/v1/sandboxes/{sandboxID}", s.handleDeleteSandbox)
	s.router.HandleFunc("/health", s.handleHealth)
	s.router.HandleFunc("/api/sandbox/suspend", s.handleSuspendSandbox)
	s.router.HandleFunc("/api/sandbox/resume", s.handleResumeSandbox)
	s.router.HandleFunc("/api/sandbox/checkpoint", s.handleCheckpointSandbox)

	s.router.HandleFunc("/api/template/create", s.handleCreateTemplate)
	s.router.HandleFunc("/api/template/pull", s.handlePullTemplate)
	s.router.HandleFunc("/api/template/push", s.handlePushTemplate)
	s.router.HandleFunc("/api/template/list", s.handleListTemplate)
	s.router.HandleFunc("/api/template/inspect", s.handleInspectTemplate)
	s.router.HandleFunc("/api/template/remove", s.handleRemoveTemplate)

	s.router.HandleFunc("/api/snapshot/list", s.handleListSnapshot)
	s.router.HandleFunc("/api/snapshot/remove", s.handleRemoveSnapshot)
	s.router.HandleFunc("/api/snapshot/info", s.handleSnapshotInfo)

	s.router.HandleFunc("/api/image/pull", s.handlePullImage)
	s.router.HandleFunc("/api/image/push", s.handlePushImage)
	s.router.HandleFunc("/api/image/list", s.handleListImage)
	s.router.HandleFunc("/api/image/remove", s.handleRemoveImage)
	s.router.HandleFunc("/api/image/unpack", s.handleUnpackImage)
	s.router.HandleFunc("/api/image/import", s.handleImportImage)
}
func (s *Daemon) Start(addr string, unixSocket string) error {
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
	// Listener is bound, so clients can connect before Serve accepts.
	util.NotifyReady()
	err = s.httpServer.Serve(ln)
	if err == http.ErrServerClosed {
		logger.Info("Main server gracefully stopped")
		err = nil
	}
	return err
}

func (s *Daemon) Cleanup() {
	s.Shutdown()
}

func (s *Daemon) Shutdown() {
	logger := ulog.GetLogger()

	s.cleanupOnce.Do(func() {
		finishShutdown := cleanupdiag.Start("daemon.shutdown")
		defer finishShutdown(nil)

		// Report deactivating while cleanup runs; TimeoutStopSec still applies.
		util.NotifyStopping()

		// stop httpServer
		if s.httpServer != nil {
			finish := cleanupdiag.Start("daemon.http.shutdown", ulog.F("timeout", shutdownTimeout.String()))
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			err := s.httpServer.Shutdown(shutdownCtx)
			shutdownCancel()
			finish(err)
			if err != nil {
				logger.Error("HTTP server shutdown error", ulog.F("error", err))
			} else {
				logger.Info("HTTP server gracefully stopped")
			}
		}

		if s.unixSocketPath != "" {
			finish := cleanupdiag.Start("daemon.http.remove_socket", ulog.F("socket", s.unixSocketPath))
			err := os.Remove(s.unixSocketPath)
			if err != nil && os.IsNotExist(err) {
				err = nil
			}
			finish(err)
			if err != nil {
				logger.Error("Failed to remove unix socket", ulog.F("socket", s.unixSocketPath), ulog.F("error", err))
			} else {
				logger.Info("Removed unix socket", ulog.F("socket", s.unixSocketPath))
			}
		}

		if s.containerdHost != nil {
			finish := cleanupdiag.Start("daemon.containerd_host.close")
			err := s.containerdHost.Close()
			finish(err)
			if err != nil {
				logger.Error("Containerd cleanup error", ulog.F("error", err))
			}
		} else if s.daemonClient != nil {
			finish := cleanupdiag.Start("daemon.containerd_client.close")
			err := s.daemonClient.Close()
			finish(err)
			if err != nil {
				logger.Error("Containerd cleanup error", ulog.F("error", err))
			}
		}

		if s.stateStore != nil {
			finish := cleanupdiag.Start("daemon.state_store.close")
			err := s.stateStore.Close()
			finish(err)
			if err != nil {
				logger.Error("State store cleanup error", ulog.F("error", err))
			}
		}
		logger.Info("Cleanup completed")
	})
}

func (s *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.controlPlaneReady() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Daemon) controlPlaneReady() bool {
	return s != nil &&
		s.stateStore != nil &&
		s.containerdHost != nil &&
		s.daemonClient != nil &&
		s.runtimeService != nil &&
		s.runtimeService.Sandbox != nil &&
		s.runtimeService.Store != nil
}

func (s *Daemon) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling create sandbox request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sandboxCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	result, err := s.runtimeService.CreateSandbox(r.Context(), runtimeapi.SandboxCreateOptions{
		Namespace:    req.Namespace,
		SandboxID:    req.SandboxID,
		LeaseID:      req.LeaseID,
		TemplateID:   req.TemplateID,
		VMMName:      req.VMMName,
		VCPUNum:      req.VCPUNum,
		VCPUMax:      req.VCPUMax,
		RamMB:        req.RAMMB,
		VolumeMounts: req.VolumeMounts,
		Env:          req.Env,
	})
	if err != nil {
		if errors.Is(err, conchruntime.ErrTemplateIDRequired) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "error",
				"error":  err.Error(),
			})
			return
		}
		logger.Error("Failed to create sandbox",
			ulog.F("sandbox_id", req.SandboxID),
			ulog.F("error", err),
		)
		http.Error(w, "Failed to create sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Sandbox created successfully",
		ulog.F("sandbox_id", result.SandboxID),
		ulog.F("namespace", result.Namespace),
		ulog.F("peer_ip", result.IP),
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sandboxResponseFromCreate(result))
}

func (s *Daemon) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	logger := ulog.GetLogger()
	logger.Debug("Handling delete sandbox request", ulog.F("sandbox_id", sandboxID))

	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if sandboxID == "" {
		http.Error(w, "Missing sandbox id", http.StatusBadRequest)
		return
	}
	if s.stateStore == nil {
		http.Error(w, "State store unavailable", http.StatusServiceUnavailable)
		return
	}

	namespace := s.resolveNamespace(r.URL.Query().Get("namespace"))
	record, err := s.findSandboxRecord(r.Context(), sandboxID, namespace)
	if err != nil {
		logger.Error("Failed to resolve sandbox for deletion",
			ulog.F("sandbox_id", sandboxID),
			ulog.F("error", err),
		)
		http.Error(w, "Failed to delete sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if record == nil {
		http.Error(w, "Sandbox not found", http.StatusNotFound)
		return
	}
	if err := s.runtimeService.RemoveSandbox(r.Context(), record.Namespace, record.SandboxID); err != nil {
		logger.Error("Failed to delete sandbox", ulog.F("sandbox_id", sandboxID), ulog.F("error", err))
		http.Error(w, "Failed to delete sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Sandbox deleted successfully", ulog.F("sandbox_id", sandboxID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Daemon) handleListSandbox(w http.ResponseWriter, r *http.Request) {
	if s.stateStore == nil {
		http.Error(w, "State store unavailable", http.StatusServiceUnavailable)
		return
	}
	namespace := s.resolveNamespace(r.URL.Query().Get("namespace"))
	states, err := parseSandboxStates(r.URL.Query()["state"])
	if err != nil {
		http.Error(w, "Invalid state filter: "+err.Error(), http.StatusBadRequest)
		return
	}
	limit, err := parseSandboxListLimit(r.URL.Query().Get("limit"))
	if err != nil {
		http.Error(w, "Invalid limit", http.StatusBadRequest)
		return
	}
	records, err := s.stateStore.ListSandboxes(r.Context())
	if err != nil {
		http.Error(w, "Failed to list sandboxes: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sandboxes := make([]sandboxInspectResponse, 0, len(records))
	for _, record := range records {
		if record.Namespace == namespace && matchesSandboxState(record, states) {
			sandboxes = append(sandboxes, sandboxResponseFromRecord(record, false))
		}
	}
	if len(sandboxes) > limit {
		sandboxes = sandboxes[:limit]
	}
	writeJSON(w, sandboxes)
}

func (s *Daemon) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	if s.stateStore == nil {
		http.Error(w, "State store unavailable", http.StatusServiceUnavailable)
		return
	}
	record, err := s.findSandboxRecord(r.Context(), sandboxID, s.resolveNamespace(r.URL.Query().Get("namespace")))
	if err != nil {
		http.Error(w, "Failed to get sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if record == nil {
		http.Error(w, "Sandbox not found", http.StatusNotFound)
		return
	}
	writeJSON(w, sandboxResponseFromRecord(*record, true))
}

func (s *Daemon) findSandboxRecord(ctx context.Context, sandboxID, namespace string) (*state.SandboxRecord, error) {
	records, err := s.stateStore.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range records {
		if records[i].SandboxID != sandboxID {
			continue
		}
		if namespace != "" && records[i].Namespace != namespace {
			continue
		}
		return &records[i], nil
	}
	return nil, nil
}

func (s *Daemon) resolveNamespace(namespace string) string {
	if namespace = strings.TrimSpace(namespace); namespace != "" {
		return namespace
	}
	if s.daemonClient != nil {
		if namespace = strings.TrimSpace(s.daemonClient.DefaultNamespace()); namespace != "" {
			return namespace
		}
	}
	return "default"
}

func parseSandboxStates(values []string) (map[string]bool, error) {
	if len(values) == 0 {
		return nil, nil
	}
	states := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "running" && value != "paused" {
			return nil, fmt.Errorf("unsupported state %q", value)
		}
		states[value] = true
	}
	return states, nil
}

func parseSandboxListLimit(raw string) (int, error) {
	if raw == "" {
		return 100, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 5000 {
		return 0, fmt.Errorf("limit must be between 1 and 5000")
	}
	return limit, nil
}

func matchesSandboxState(record state.SandboxRecord, states map[string]bool) bool {
	running := record.State == state.SandboxReady
	paused := record.State == state.SandboxSuspended
	if len(states) == 0 {
		return running || paused
	}
	return states["running"] && running || states["paused"] && paused
}

func sandboxResponseFromRecord(record state.SandboxRecord, detailed bool) sandboxInspectResponse {
	response := sandboxInspectResponse{
		TemplateID:   record.SourceTemplateID,
		ImageName:    "",
		SnapshotID:   "",
		SandboxID:    record.SandboxID,
		Namespace:    record.Namespace,
		StartedAt:    formatUnixNanoRFC3339(record.CreatedAt),
		CPUCount:     record.VCPUNum,
		MemoryMB:     record.RamMB,
		Alias:        "",
		Metadata:     map[string]string{},
		VolumeMounts: []sandboxVolumeMountResponse{},
	}
	if response.Metadata == nil {
		response.Metadata = map[string]string{}
	}
	if detailed {
		domain := record.IP
		response.Domain = &domain
		response.Lifecycle = &sandboxLifecycleResponse{}
	}
	return response
}

func sandboxResponseFromCreate(result runtimeapi.SandboxCreateResult) createSandboxResponse {
	// TODO: populate conchInitVersion and alias when runtime support is available.
	return createSandboxResponse{
		TemplateID:           result.TemplateID,
		SandboxID:            result.SandboxID,
		Namespace:            result.Namespace,
		ConchInitAccessToken: result.AgentToken,
		Domain:               result.IP,
	}
}

func formatUnixNanoRFC3339(timestamp int64) string {
	if timestamp <= 0 {
		return ""
	}
	return time.Unix(0, timestamp).UTC().Format(time.RFC3339)
}

func (s *Daemon) handleSuspendSandbox(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling suspend sandbox request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sandboxLifecycleRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	record, err := s.findSandboxRecord(r.Context(), req.SandboxID, s.resolveNamespace(req.Namespace))
	if err != nil {
		http.Error(w, "Failed to resolve sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if record == nil {
		http.Error(w, "Sandbox not found", http.StatusNotFound)
		return
	}
	err = s.runtimeService.SuspendSandbox(r.Context(), record.Namespace, record.SandboxID)
	if err != nil {
		logger.Error("Failed to suspend sandbox",
			ulog.F("sandbox_id", req.SandboxID),
			ulog.F("error", err),
		)
		http.Error(w, "Failed to suspend sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Sandbox suspended successfully", ulog.F("sandbox_id", req.SandboxID))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Daemon) handleResumeSandbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req sandboxLifecycleRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	record, err := s.findSandboxRecord(r.Context(), req.SandboxID, s.resolveNamespace(req.Namespace))
	if err != nil {
		http.Error(w, "Failed to resolve sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if record == nil {
		http.Error(w, "Sandbox not found", http.StatusNotFound)
		return
	}
	if err := s.runtimeService.ResumeSandbox(r.Context(), record.Namespace, record.SandboxID); err != nil {
		http.Error(w, "Failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Daemon) handleCheckpointSandbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req sandboxCheckpointRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	result, err := s.runtimeService.CheckpointSandbox(r.Context(), runtimeapi.SandboxCheckpointOptions{
		Namespace: req.Namespace,
		SandboxID: req.SandboxID,
		Labels:    req.Labels,
	})
	if err != nil {
		http.Error(w, "Failed to checkpoint sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":            "ok",
		"template_id":       result.TemplateID,
		"boot_index_digest": result.BootIndexDigest,
	})
}

func (s *Daemon) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "Invalid multipart body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req templateCreateRequest
	if err := decodeStrictJSON(strings.NewReader(r.FormValue("metadata")), &req); err != nil {
		http.Error(w, "Invalid metadata: "+err.Error(), http.StatusBadRequest)
		return
	}
	tmpDir, err := os.MkdirTemp("", "conch-template-api-*")
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
	result, err := s.createTemplate(r.Context(), req, kernelPath, initrdPath)
	if err != nil {
		http.Error(w, "Failed to create template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{
		"status":            "ok",
		"template_id":       result.TemplateID,
		"boot_index_digest": result.BootIndexDigest,
		"boot_index_tag":    result.BootIndexTag,
	})
}

func (s *Daemon) createTemplate(ctx context.Context, req templateCreateRequest, kernelPath, initrdPath string) (runtimeapi.TemplateCreateResult, error) {
	return s.runtimeService.CreateTemplate(ctx, runtimeapi.TemplateCreateOptions{
		Namespace:    req.Namespace,
		Source:       req.Source,
		KernelPath:   kernelPath,
		InitrdPath:   initrdPath,
		BootIndexTag: req.BootIndexTag,
		PlainHTTP:    req.PlainHTTP,
		Username:     req.Username,
		Password:     req.Password,
		Labels:       req.Labels,
	})
}

func (s *Daemon) handlePullTemplate(w http.ResponseWriter, r *http.Request) {
	var req templatePullRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	result, err := s.runtimeService.PullTemplate(r.Context(), runtimeapi.TemplatePullOptions{
		Reference: req.Reference,
		Namespace: req.Namespace,
		PlainHTTP: req.PlainHTTP,
		Username:  req.Username,
		Password:  req.Password,
		Labels:    req.Labels,
	})
	if err != nil {
		http.Error(w, "Failed to pull template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{
		"status":            "ok",
		"template_id":       result.TemplateID,
		"boot_index_digest": result.BootIndexDigest,
		"build_ref":         result.BuildRef,
	})
}

func (s *Daemon) handlePushTemplate(w http.ResponseWriter, r *http.Request) {
	var req templatePushRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	if err := s.runtimeService.PushTemplate(r.Context(), runtimeapi.TemplatePushOptions{
		TemplateID:      req.TemplateID,
		RemoteReference: req.RemoteReference,
		Namespace:       req.Namespace,
		PlainHTTP:       req.PlainHTTP,
		Username:        req.Username,
		Password:        req.Password,
		RegistryTimeout: req.RegistryTimeout,
	}); err != nil {
		http.Error(w, "Failed to push template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Daemon) handleListTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateListRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	items, err := s.runtimeService.ListTemplates(r.Context(), runtimeapi.TemplateListOptions{
		Namespace: req.Namespace,
		Origin:    req.Origin,
		BootMode:  req.BootMode,
	})
	if err != nil {
		http.Error(w, "Failed to list templates: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, templateListResponse{Items: items})
}

func (s *Daemon) handleInspectTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateIDRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	item, err := s.runtimeService.GetTemplate(r.Context(), req.ID)
	if err != nil {
		http.Error(w, "Failed to inspect template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, item)
}

func (s *Daemon) handleRemoveTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateIDRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	if err := s.runtimeService.RemoveTemplate(r.Context(), req.ID); err != nil {
		http.Error(w, "Failed to remove template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Daemon) handlePullImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling pull image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Image == nil {
		http.Error(w, "Image service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req pullImageRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	opts := runtimeapi.PullImageOptions{
		ImageName:  req.ImageName,
		Namespace:  req.Namespace,
		PlainHTTP:  req.PlainHTTP,
		Username:   req.Username,
		Password:   req.Password,
		SkipUnpack: req.SkipUnpack,
	}

	result, err := s.runtimeService.PullImage(r.Context(), opts)
	if err != nil {
		logger.Error("Failed to pull image",
			ulog.F("image_name", opts.ImageName),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to pull image", err)
		return
	}

	logger.Info("Image pulled successfully", ulog.F("image_name", opts.ImageName))
	writeImageResults(w, result.Refs)
}

func (s *Daemon) handlePushImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling push image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Image == nil {
		http.Error(w, "Image service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req pushImageRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	opts := runtimeapi.PushImageOptions{
		LocalImage:      req.LocalImage,
		RemoteImage:     req.RemoteImage,
		Namespace:       req.Namespace,
		PlainHTTP:       req.PlainHTTP,
		Username:        req.Username,
		Password:        req.Password,
		RegistryTimeout: req.RegistryTimeout,
	}
	if err := s.runtimeService.PushImage(r.Context(), opts); err != nil {
		logger.Error("Failed to push image",
			ulog.F("local_image", opts.LocalImage),
			ulog.F("remote_image", opts.RemoteImage),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to push image", err)
		return
	}
	logger.Info("Image pushed successfully",
		ulog.F("local_image", opts.LocalImage),
		ulog.F("remote_image", opts.RemoteImage),
	)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Daemon) handleListImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling list image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Image == nil {
		http.Error(w, "Image service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req listImageRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	images, err := s.runtimeService.ListImages(r.Context(), runtimeapi.ListImagesOptions{
		Namespace: req.Namespace,
		Filters:   req.Filters,
	})
	if err != nil {
		logger.Error("Failed to list images", ulog.F("error", err))
		writeImageError(w, "Failed to list images", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listImageResponse{Images: imageRecordResponses(images)})
}

func (s *Daemon) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling remove image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Image == nil {
		http.Error(w, "Image service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req removeImageRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	opts := runtimeapi.RemoveImageOptions{
		Namespace:   req.Namespace,
		ImageName:   req.ImageName,
		Synchronous: req.Synchronous,
	}
	if err := s.runtimeService.RemoveImage(r.Context(), opts); err != nil {
		logger.Error("Failed to remove image",
			ulog.F("image_name", opts.ImageName),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to remove image", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Daemon) handleUnpackImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling unpack image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Image == nil {
		http.Error(w, "Image service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req unpackImageRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	opts := runtimeapi.UnpackImageOptions{
		ImageName: req.ImageName,
		Namespace: req.Namespace,
	}
	results, err := s.runtimeService.UnpackImage(r.Context(), opts)
	if err != nil {
		logger.Error("Failed to unpack image",
			ulog.F("image_name", opts.ImageName),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to unpack image", err)
		return
	}

	logger.Info("Image unpacked successfully", ulog.F("image_name", opts.ImageName))
	writeImageResults(w, results)
}

func (s *Daemon) handleImportImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling import image request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Image == nil {
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

	resp, err := s.runtimeService.ImportImageArchive(r.Context(), file, runtimeapi.ImportImageArchiveOptions{
		Namespace:   r.FormValue("namespace"),
		ImportedTag: r.FormValue("imported_tag"),
	})
	if err != nil {
		logger.Error("Failed to import image archive", ulog.F("error", err))
		writeImageError(w, "Failed to import image archive", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(importImageArchiveHTTPResponse(resp))
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

func decodePostJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return decodeJSONBody(w, r, out)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := decodeStrictJSON(r.Body, out); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func decodeStrictJSON(reader io.Reader, out any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain a single JSON value")
		}
		return fmt.Errorf("invalid trailing data: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeImageResults(w http.ResponseWriter, results map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"results": results,
	})
}

func writeImageError(w http.ResponseWriter, prefix string, err error) {
	status := http.StatusInternalServerError
	detail := err.Error()
	if errors.Is(err, conchimage.ErrInvalidRequest) || errors.Is(err, conchimage.ErrOCIConversionFailed) {
		status = http.StatusBadRequest
	} else {
		var registryErr remoteerrors.ErrUnexpectedStatus
		if errors.As(err, &registryErr) {
			if registryErr.StatusCode >= http.StatusContinue && registryErr.StatusCode <= 599 {
				status = registryErr.StatusCode
			}
			if text := http.StatusText(registryErr.StatusCode); text != "" {
				detail = "registry request failed: " + text
			} else {
				detail = "registry request failed"
			}
		}
	}
	http.Error(w, prefix+": "+detail, status)
}

func (s *Daemon) handleSnapshotInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req snapshotInfoRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Snapshot == nil {
		http.Error(w, "Snapshot service unavailable", http.StatusServiceUnavailable)
		return
	}

	info, err := s.runtimeService.SnapshotInfo(r.Context(), runtimeapi.SnapshotInfoOptions{
		Key:       req.Key,
		Namespace: req.Namespace,
	})
	if err != nil {
		http.Error(w, "Failed to get snapshot info: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshotRecordHTTPResponse(info))
}

func (s *Daemon) handleListSnapshot(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling list snapshot request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Snapshot == nil {
		http.Error(w, "Snapshot service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req listSnapshotRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	snapshots, err := s.runtimeService.ListSnapshots(r.Context(), runtimeapi.ListSnapshotsOptions{
		Namespace: req.Namespace,
		Filters:   req.Filters,
	})
	if err != nil {
		logger.Error("Failed to list snapshots", ulog.F("error", err))
		http.Error(w, "Failed to list snapshots: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listSnapshotResponse{Snapshots: snapshotRecordHTTPResponses(snapshots)})
}

func (s *Daemon) handleRemoveSnapshot(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling remove snapshot request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Snapshot == nil {
		http.Error(w, "Snapshot service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req removeSnapshotRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	opts := runtimeapi.RemoveSnapshotOptions{
		Key:       req.Key,
		Namespace: req.Namespace,
	}
	if err := s.runtimeService.RemoveSnapshot(r.Context(), opts); err != nil {
		logger.Error("Failed to remove snapshot",
			ulog.F("key", opts.Key),
			ulog.F("error", err),
		)
		http.Error(w, "Failed to remove snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(removeSnapshotResponse{Status: "ok"})
}
