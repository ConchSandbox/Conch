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

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/adapters/containerd/host"
	"github.com/openeuler/Conch/internal/cleanupdiag"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/runtimeapi"
	conchsandbox "github.com/openeuler/Conch/internal/sandbox"
	conchsnapshot "github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/util"
	"github.com/openeuler/Conch/internal/volume"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	shutdownTimeout               = 30 * time.Second
	minimumSandboxRAMMB           = 128
	maxJSONBodyBytes              = 1 << 20
	maxTemplateFileBytes          = 64 << 20 // 64 MB
	maxTemplateMultipartBodyBytes = 2*maxTemplateFileBytes + maxJSONBodyBytes
	serverReadHeaderTimeout       = 10 * time.Second
	serverIdleTimeout             = 60 * time.Second
	serverMaxHeaderBytes          = 64 << 10
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

	store, err := state.OpenBolt(cfg.StatePath())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open state store: %w", err)
	}
	s.stateStore = store
	logger.Info("State store initialized", ulog.F("path", cfg.StatePath()))
	s.volumeManager, err = volume.NewManager(volume.Config{
		MaxMounts: cfg.Volume.MaxMounts,
		Backend:   cfg.Volume.Backend,
		Virtiofs: volume.VirtiofsConfig{
			Binary:     cfg.Volume.Virtiofs.Binary,
			RuntimeDir: cfg.VirtiofsRuntimeDir(),
		},
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("init volume manager: %w", err)
	}

	host, err := containerdhost.Start(ctx, containerdhost.Config{
		RootDir:  cfg.ContainerdRootDir(),
		StateDir: cfg.ContainerdStateDir(),
		Snapshot: containerdhost.SnapshotConfig{
			WorkDir: cfg.Server.WorkDir,
		},
		Sandbox: &conchsandbox.Config{
			Network: netstack.PoolConfig{
				WarmPoolSize: cfg.Network.WarmPoolSize,
				CNI:          cfg.Network.CNI,
			},
			VMMBinaries:        cfg.Sandbox.BinaryPaths(),
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

	s.runtimeService = conchruntime.New(host.SandboxManager(), host.Client(), store)
	s.runtimeService.Snapshot = host.SnapshotServer()
	s.runtimeService.Templates = host.TemplateStore()
	s.runtimeService.SetSandboxDefaults(runtimeapi.SandboxDefaults{
		TemplateID: cfg.Sandbox.DefaultSpec.TemplateID,
		VMMName:    cfg.Sandbox.Backend,
		VCPUNum:    cfg.Sandbox.DefaultSpec.VCPUNum,
		VCPUMax:    cfg.Sandbox.DefaultSpec.VCPUMax,
		RamMB:      cfg.Sandbox.DefaultSpec.RamMB,
	})

	manager := host.SandboxManager()
	if manager != nil {
		records, err := store.ListSandboxes(ctx)
		if err != nil {
			cleanupErr := host.Close()
			_ = store.Close()
			cancel()
			return nil, errors.Join(fmt.Errorf("list stale sandboxes during startup: %w", err), cleanupErr)
		}
		sandboxIDs := make([]string, 0, len(records))
		vmmPIDs := make([]int, 0, len(records))
		hasCreatingSandbox := false
		for _, record := range records {
			sandboxIDs = append(sandboxIDs, record.SandboxID)
			if record.State == state.SandboxCreating {
				hasCreatingSandbox = true
			}
			if record.VMMPID > 0 {
				vmmPIDs = append(vmmPIDs, record.VMMPID)
			}
		}
		logger.Info("cleaning up resources from abnormal sandbox exits")
		if err := manager.RecoverStaleResources(ctx, sandboxIDs, vmmPIDs, hasCreatingSandbox); err != nil {
			cleanupErr := host.Close()
			_ = store.Close()
			cancel()
			return nil, errors.Join(fmt.Errorf("recover stale sandbox resources during startup: %w", err), cleanupErr)
		}
		if err := s.removeAllSandboxes(); err != nil {
			cleanupErr := host.Close()
			_ = store.Close()
			cancel()
			return nil, errors.Join(fmt.Errorf("clean up stale sandboxes during startup: %w", err), cleanupErr)
		}
		if err := manager.Start(ctx); err != nil {
			cleanupErr := host.Close()
			_ = store.Close()
			cancel()
			return nil, errors.Join(fmt.Errorf("start network pool during startup: %w", err), cleanupErr)
		}
	}

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
	s.router.HandleFunc("PUT /api/v1/sandboxes/{sandboxID}/network", s.handleUpdateSandboxNetwork)
	s.router.HandleFunc("/health", s.handleHealth)
	s.router.HandleFunc("/api/sandbox/suspend", s.handleSuspendSandbox)
	s.router.HandleFunc("/api/sandbox/resume", s.handleResumeSandbox)
	s.router.HandleFunc("/api/sandbox/checkpoint", s.handleCheckpointSandbox)

	s.router.HandleFunc("/api/template/create", s.handleCreateTemplate)
	s.router.HandleFunc("/api/template/pull", s.handlePullTemplate)
	s.router.HandleFunc("/api/template/push", s.handlePushTemplate)
	s.router.HandleFunc("/api/template/unpack", s.handleUnpackTemplate)
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
}
func (s *Daemon) Start(unixSocket string) error {
	logger := ulog.GetLogger()

	if unixSocket == "" {
		return errors.New("unix socket path is required")
	}

	// First create the parent directory if needed; this requires permission for the socket path.
	// Then for any existing stale socket it should be removed before start to listen
	if err := os.MkdirAll(filepath.Dir(unixSocket), 0o755); err != nil {
		return fmt.Errorf("failed to create unix socket directory: %w", err)
	}
	if err := os.Remove(unixSocket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove stale unix socket: %w", err)
	}

	ln, err := net.Listen("unix", unixSocket)
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

	s.listener = ln
	s.httpServer = &http.Server{
		Handler:           s.router,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    serverMaxHeaderBytes,
	}
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

		if s.runtimeService != nil && s.stateStore != nil {
			finish := cleanupdiag.Start("daemon.sandboxes.remove_all")
			err := s.removeAllSandboxes()
			finish(err)
			if err != nil {
				logger.Error("Sandbox cleanup error", ulog.F("error", err))
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

// removeAllSandboxes removes runtime resources and persistent records.
func (s *Daemon) removeAllSandboxes() error {
	records, err := s.stateStore.ListSandboxes(context.Background())
	if err != nil {
		return fmt.Errorf("list sandboxes for shutdown: %w", err)
	}

	var errs []error
	for _, record := range records {
		if err := s.runtimeService.RemoveSandbox(context.Background(), record.SandboxID); err != nil {
			errs = append(errs, fmt.Errorf("remove sandbox %s: %w", record.SandboxID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	if !s.controlPlaneReady() {
		writeAPIError(w, errServiceUnavailable.New())
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
		writeMethodNotAllowed(w)
		return
	}

	var req sandboxCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	defer ulog.TraceCost(ulog.TraceStart(), req.SandboxID, "handleCreateSandbox()")
	if req.RAMMB != 0 && req.RAMMB < minimumSandboxRAMMB {
		message := fmt.Sprintf("ram_mb must be at least %d, got %d", minimumSandboxRAMMB, req.RAMMB)
		writeAPIError(w, conchsandbox.ErrInvalidArgument.WrapMessage(errors.New(message), message))
		return
	}
	if s.runtimeService == nil || s.runtimeService.Sandbox == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	result, err := s.runtimeService.CreateSandbox(r.Context(), runtimeapi.SandboxCreateOptions{
		SandboxID:    req.SandboxID,
		LeaseID:      req.LeaseID,
		TemplateID:   req.TemplateID,
		VMMName:      req.VMMName,
		VCPUNum:      req.VCPUNum,
		VCPUMax:      req.VCPUMax,
		RamMB:        req.RAMMB,
		VolumeMounts: req.VolumeMounts,
		Env:          req.Env,
		Network:      req.Network,
	})
	if err != nil {
		writeAPIError(w, err, ulog.F("operation", "sandbox.create"), ulog.F("sandbox_id", req.SandboxID))
		return
	}

	logger.Info("Sandbox created successfully",
		ulog.F("sandbox_id", result.SandboxID),
		ulog.F("peer_ip", result.IP),
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sandboxResponseFromCreate(result))
}

func (s *Daemon) handleUpdateSandboxNetwork(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	if sandboxID == "" {
		writeAPIError(w, conchsandbox.ErrInvalidArgument.Wrap(errors.New("sandbox id is required")))
		return
	}
	var req runtimeapi.SandboxNetworkConfig
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if s.stateStore == nil || s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	record, err := s.findSandboxRecord(r.Context(), sandboxID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if record == nil {
		writeAPIError(w, conchsandbox.ErrNotFound.New())
		return
	}
	if err := s.runtimeService.UpdateSandboxNetworkConfig(r.Context(), runtimeapi.SandboxNetworkUpdateOptions{
		SandboxID: record.SandboxID,
		Network:   &req,
	}); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Daemon) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	logger := ulog.GetLogger()
	logger.Debug("Handling delete sandbox request", ulog.F("sandbox_id", sandboxID))

	if r.Method != http.MethodDelete {
		writeMethodNotAllowed(w)
		return
	}
	if sandboxID == "" {
		writeAPIError(w, conchsandbox.ErrInvalidArgument.Wrap(errors.New("sandbox id is required")))
		return
	}
	if s.stateStore == nil || s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}

	record, err := s.findSandboxRecord(r.Context(), sandboxID)
	if err != nil {
		writeAPIError(w, err, ulog.F("operation", "sandbox.delete"), ulog.F("sandbox_id", sandboxID))
		return
	}
	if record == nil {
		writeAPIError(w, conchsandbox.ErrNotFound.New())
		return
	}
	if err := s.runtimeService.RemoveSandbox(r.Context(), record.SandboxID); err != nil {
		writeAPIError(w, err, ulog.F("operation", "sandbox.delete"), ulog.F("sandbox_id", sandboxID))
		return
	}

	logger.Info("Sandbox deleted successfully", ulog.F("sandbox_id", sandboxID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Daemon) handleListSandbox(w http.ResponseWriter, r *http.Request) {
	if s.stateStore == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	states, err := parseSandboxStates(r.URL.Query()["state"])
	if err != nil {
		writeAPIError(w, conchsandbox.ErrInvalidArgument.WrapMessage(err, "invalid sandbox state filter"))
		return
	}
	limit, err := parseSandboxListLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(w, conchsandbox.ErrInvalidArgument.WrapMessage(err, "invalid sandbox list limit"))
		return
	}
	records, err := s.stateStore.ListSandboxes(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	sandboxes := make([]sandboxInspectResponse, 0, len(records))
	for _, record := range records {
		if matchesSandboxState(record, states) {
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
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	record, err := s.findSandboxRecord(r.Context(), sandboxID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if record == nil {
		writeAPIError(w, conchsandbox.ErrNotFound.New())
		return
	}
	writeJSON(w, sandboxResponseFromRecord(*record, true))
}

func (s *Daemon) findSandboxRecord(ctx context.Context, sandboxID string) (*state.SandboxRecord, error) {
	records, err := s.stateStore.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range records {
		if records[i].SandboxID != sandboxID {
			continue
		}
		return &records[i], nil
	}
	return nil, nil
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
		response.Network = record.Network
	}
	return response
}

func sandboxResponseFromCreate(result runtimeapi.SandboxCreateResult) createSandboxResponse {
	// TODO: populate conchInitVersion and alias when runtime support is available.
	return createSandboxResponse{
		TemplateID:           result.TemplateID,
		SandboxID:            result.SandboxID,
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
		writeMethodNotAllowed(w)
		return
	}

	var req sandboxLifecycleRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeAPIError(w, conchsandbox.ErrInvalidArgument.Wrap(errors.New("sandbox_id is required")))
		return
	}
	if s.stateStore == nil || s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}

	record, err := s.findSandboxRecord(r.Context(), req.SandboxID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if record == nil {
		writeAPIError(w, conchsandbox.ErrNotFound.New())
		return
	}
	err = s.runtimeService.SuspendSandbox(r.Context(), record.SandboxID)
	if err != nil {
		writeAPIError(w, err, ulog.F("operation", "sandbox.suspend"), ulog.F("sandbox_id", req.SandboxID))
		return
	}

	logger.Info("Sandbox suspended successfully", ulog.F("sandbox_id", req.SandboxID))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Daemon) handleResumeSandbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	var req sandboxLifecycleRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeAPIError(w, conchsandbox.ErrInvalidArgument.Wrap(errors.New("sandbox_id is required")))
		return
	}
	if s.stateStore == nil || s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	record, err := s.findSandboxRecord(r.Context(), req.SandboxID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if record == nil {
		writeAPIError(w, conchsandbox.ErrNotFound.New())
		return
	}
	if err := s.runtimeService.ResumeSandbox(r.Context(), record.SandboxID); err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Daemon) handleCheckpointSandbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	var req sandboxCheckpointRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeAPIError(w, conchsandbox.ErrInvalidArgument.Wrap(errors.New("sandbox_id is required")))
		return
	}
	if s.runtimeService == nil || s.runtimeService.Sandbox == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	result, err := s.runtimeService.CheckpointSandbox(r.Context(), runtimeapi.SandboxCheckpointOptions{
		SandboxID: req.SandboxID,
		Labels:    req.Labels,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":      "ok",
		"template_id": result.TemplateID,
	})
}

func (s *Daemon) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTemplateMultipartBodyBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(w, errRequestBodyTooLarge.Wrap(err))
			return
		}
		writeAPIError(w, errRequestInvalidMultipart.Wrap(err))
		return
	}
	defer r.MultipartForm.RemoveAll()
	var req templateCreateRequest
	if err := decodeStrictJSON(strings.NewReader(r.FormValue("metadata")), &req); err != nil {
		writeAPIError(w, errRequestInvalidMultipart.WrapMessage(err, "invalid multipart metadata: "+err.Error()))
		return
	}
	tmpDir, err := os.MkdirTemp("", "conch-template-api-*")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer os.RemoveAll(tmpDir)
	kernelPath, err := saveMultipartFile(r, "kernel", tmpDir, "kernel")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	initrdPath, err := saveMultipartFile(r, "initrd", tmpDir, "initrd")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	result, err := s.createTemplate(r.Context(), req, kernelPath, initrdPath)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, map[string]string{
		"status":      "ok",
		"template_id": result.TemplateID,
		"build_ref":   result.BuildRef,
	})
}

func (s *Daemon) createTemplate(ctx context.Context, req templateCreateRequest, kernelPath, initrdPath string) (runtimeapi.TemplateCreateResult, error) {
	return s.runtimeService.CreateTemplate(ctx, runtimeapi.TemplateCreateOptions{
		Source:     req.Source,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		PlainHTTP:  req.PlainHTTP,
		Username:   req.Username,
		Password:   req.Password,
		Labels:     req.Labels,
	})
}

func (s *Daemon) handlePullTemplate(w http.ResponseWriter, r *http.Request) {
	var req templatePullRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	if s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	result, err := s.runtimeService.PullTemplate(r.Context(), runtimeapi.TemplatePullOptions{
		Reference: req.Reference,
		PlainHTTP: req.PlainHTTP,
		Username:  req.Username,
		Password:  req.Password,
		Labels:    req.Labels,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, map[string]string{
		"status":      "ok",
		"template_id": result.TemplateID,
		"build_ref":   result.BuildRef,
	})
}

func (s *Daemon) handlePushTemplate(w http.ResponseWriter, r *http.Request) {
	var req templatePushRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	if s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	if err := s.runtimeService.PushTemplate(r.Context(), runtimeapi.TemplatePushOptions{
		TemplateID:      req.TemplateID,
		RemoteReference: req.RemoteReference,
		PlainHTTP:       req.PlainHTTP,
		Username:        req.Username,
		Password:        req.Password,
	}); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Daemon) handleUnpackTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateUnpackRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	if s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	if err := s.runtimeService.UnpackTemplate(r.Context(), runtimeapi.TemplateUnpackOptions{
		TemplateID: req.TemplateID,
	}); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Daemon) handleListTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateListRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	if s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	items, err := s.runtimeService.ListTemplates(r.Context(), runtimeapi.TemplateListOptions{
		Origin:   req.Origin,
		BootMode: req.BootMode,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, templateListResponse{Items: items})
}

func (s *Daemon) handleInspectTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateIDRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	if s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	item, err := s.runtimeService.GetTemplate(r.Context(), req.ID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, item)
}

func (s *Daemon) handleRemoveTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateIDRequest
	if !decodePostJSON(w, r, &req) {
		return
	}
	if s.runtimeService == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}
	if err := s.runtimeService.RemoveTemplate(r.Context(), req.ID); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Daemon) handlePullImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling pull image request")

	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if s.daemonClient == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}

	var req pullImageRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	opts := runtimeapi.PullImageOptions{
		ImageName: req.ImageName,
		PlainHTTP: req.PlainHTTP,
		Username:  req.Username,
		Password:  req.Password,
	}

	if err := conchimage.Pull(r.Context(), s.daemonClient, opts); err != nil {
		writeAPIError(w, err, ulog.F("operation", "image.pull"), ulog.F("image_name", opts.ImageName))
		return
	}

	logger.Info("Image pulled successfully", ulog.F("image_name", opts.ImageName))
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Daemon) handlePushImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling push image request")

	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if s.daemonClient == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}

	var req pushImageRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	opts := runtimeapi.PushImageOptions{
		LocalImage:  req.LocalImage,
		RemoteImage: req.RemoteImage,
		PlainHTTP:   req.PlainHTTP,
		Username:    req.Username,
		Password:    req.Password,
	}
	if err := conchimage.Push(r.Context(), s.daemonClient, opts); err != nil {
		writeAPIError(w, err,
			ulog.F("operation", "image.push"),
			ulog.F("local_image", opts.LocalImage),
			ulog.F("remote_image", opts.RemoteImage),
		)
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
		writeMethodNotAllowed(w)
		return
	}
	if s.daemonClient == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}

	var req listImageRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	images, err := conchimage.List(r.Context(), s.daemonClient, runtimeapi.ListImagesOptions{
		Filters: req.Filters,
	})
	if err != nil {
		writeAPIError(w, err, ulog.F("operation", "image.list"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listImageResponse{Images: images})
}

func (s *Daemon) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling remove image request")

	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if s.daemonClient == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}

	var req removeImageRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	opts := runtimeapi.RemoveImageOptions{
		ImageName:   req.ImageName,
		Synchronous: req.Synchronous,
	}
	if err := conchimage.Remove(r.Context(), s.daemonClient, opts); err != nil {
		writeAPIError(w, err, ulog.F("operation", "image.remove"), ulog.F("image_name", opts.ImageName))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func saveMultipartFile(r *http.Request, field, dir, name string) (string, error) {
	file, _, err := r.FormFile(field)
	if err != nil {
		return "", errRequestInvalidMultipart.WrapMessage(err, "invalid multipart field: "+field)
	}
	defer file.Close()
	path := filepath.Join(dir, name)
	if err := writeMultipartFile(path, file); err != nil {
		return "", fmt.Errorf("save multipart field %s: %w", field, err)
	}
	return path, nil
}

func writeMultipartFile(path string, file multipart.File) error {
	return writeLimitedFile(path, file, maxTemplateFileBytes)
}

func writeLimitedFile(path string, reader io.Reader, maxBytes int64) (retErr error) {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); retErr == nil && err != nil {
			retErr = err
		}
		if retErr != nil {
			_ = os.Remove(path)
		}
	}()
	written, err := io.Copy(out, io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return err
	}
	if written > maxBytes {
		return errRequestBodyTooLarge.WrapMessage(
			fmt.Errorf("multipart file exceeds maximum size %d bytes", maxBytes),
			fmt.Sprintf("multipart file exceeds maximum size %d bytes", maxBytes),
		)
	}
	return nil
}

func decodePostJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return false
	}
	return decodeJSONBody(w, r, out)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	if err := decodeStrictJSON(r.Body, out); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(w, errRequestBodyTooLarge.Wrap(err))
			return false
		}
		writeAPIError(w, errRequestInvalidBody.WrapMessage(err, "invalid request body: "+err.Error()))
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

func (s *Daemon) handleSnapshotInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	var req snapshotInfoRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Key == "" {
		writeAPIError(w, conchsnapshot.ErrInvalidArgument.WrapMessage(errors.New("key is required"), "key is required"))
		return
	}
	if s.runtimeService == nil || s.runtimeService.Snapshot == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}

	info, err := s.runtimeService.SnapshotInfo(r.Context(), runtimeapi.SnapshotInfoOptions{
		Key: req.Key,
	})
	if err != nil {
		writeAPIError(w, err, ulog.F("operation", "snapshot.info"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (s *Daemon) handleListSnapshot(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling list snapshot request")

	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Snapshot == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}

	var req listSnapshotRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	snapshots, err := s.runtimeService.ListSnapshots(r.Context(), runtimeapi.ListSnapshotsOptions{
		Filters: req.Filters,
	})
	if err != nil {
		writeAPIError(w, err, ulog.F("operation", "snapshot.list"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listSnapshotResponse{Snapshots: snapshots})
}

func (s *Daemon) handleRemoveSnapshot(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling remove snapshot request")

	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Snapshot == nil {
		writeAPIError(w, errServiceUnavailable.New())
		return
	}

	var req removeSnapshotRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Key == "" {
		writeAPIError(w, conchsnapshot.ErrInvalidArgument.WrapMessage(errors.New("key is required"), "key is required"))
		return
	}
	opts := runtimeapi.RemoveSnapshotOptions{
		Key: req.Key,
	}
	if err := s.runtimeService.RemoveSnapshot(r.Context(), opts); err != nil {
		writeAPIError(w, err, ulog.F("operation", "snapshot.remove"), ulog.F("key", opts.Key))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(removeSnapshotResponse{Status: "ok"})
}
