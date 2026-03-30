package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/containerd/containerd"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandbox/network"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	shutdownTimeout = 30 * time.Second
)

type Server struct {
	router         *http.ServeMux
	sandboxManager sandboxManager
	daemonClient   *daemon.Client
	httpServer     *http.Server
	listener       net.Listener
	unixSocketPath string
	cleanupOnce    sync.Once

	// TODO: need ListCachedBuilds()
}

type sandboxManager interface {
	Create(req sandbox.SandboxCreateRequest) (string, error)
	Delete(req sandbox.SandboxDeleteRequest) error
	Pause(req sandbox.SandboxPauseRequest) (string, error)
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

	daemonClient, err := daemon.New(
		cfg.Containerd.Socket,
		containerd.WithDefaultNamespace(cfg.Containerd.DefaultNamespace),
	)
	if err != nil {
		logger.Error("Failed to init containerd manager", ulog.F("error", err))
		cancel()
		return nil, fmt.Errorf("failed to init containerd manager: %w", err)
	}
	s.daemonClient = daemonClient

	// Initialize snapshot server
	err = snapshot.NewServer(common.WorkDir, daemonClient)
	if err != nil {
		_ = daemonClient.Close()
		cancel()
		logger.Error("Failed to init snapshot manager", ulog.F("error", err))
		return nil, fmt.Errorf("failed to init snapshot manager: %w", err)
	}

	// Initialize sandbox manager
	pool, err := network.NewPool(cfg.Network.PoolSize, cfg.Network.DynamicReservation, cfg.Network.TapIP, cfg.Network.TapMask)
	if err != nil {
		logger.Error("Failed to initialize network pool; sandbox APIs will return errors", ulog.F("error", err))
		_ = daemonClient.Close()
		cancel()
		_ = snapshot.Close()
		return nil, fmt.Errorf("failed to init network pool: %w", err)
	}

	s.SetSandboxManager(sandbox.NewManager(pool, daemonClient, cfg.Containerd.Namespace))
	go pool.Populate(ctx)

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

		if m, ok := s.sandboxManager.(*sandbox.Manager); ok {
			if err := m.CleanupPool(); err != nil {
				logger.Error("Server cleanup error", ulog.F("error", err))
			}
		}
		snapshot.CleanupAllViews()
		if err := snapshot.Close(); err != nil {
			logger.Error("Snapshot cleanup error", ulog.F("error", err))
		}
		if err := s.daemonClient.Close(); err != nil {
			logger.Error("Containerd cleanup error", ulog.F("error", err))
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

// TODO return available snapshot_id
func (s *Server) handleListSnapshot(w http.ResponseWriter, r *http.Request) {}
