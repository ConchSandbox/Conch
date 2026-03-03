package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandbox/network"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
)

const (
	workDir         = "/tmp/snapshot"
	shutdownTimeout = 30 * time.Second
)

type Server struct {
	router         *http.ServeMux
	sandboxManager sandboxManager
	daemonClient   *daemon.Client
	httpServer     *http.Server
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
				fmt.Printf("receive error: %v\n", ctx.Err())
			case sig = <-signalChannel:
				fmt.Printf("interrupted by a %v signal, process exiting\n", sig)
				cancel()
				s.Cleanup()

				return
			}
		}
	}()
	return
}

func NewServer() (*Server, error) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		router: http.NewServeMux(),
	}
	s.routes()

	daemonClient, err := daemon.New(common.ContainerdSock)
	if err != nil {
		fmt.Printf("Failed to init containerd manager: %v", err)
		cancel()
		return nil, fmt.Errorf("failed to init containerd manager: %w", err)
	}
	s.daemonClient = daemonClient

	// Initialize snapshot server
	err = snapshot.NewServer(workDir, daemonClient)
	if err != nil {
		_ = daemonClient.Close()
		cancel()
		return nil, fmt.Errorf("failed to init snapshot manager: %w", err)
	}

	// Initialize sandbox manager
	pool := network.NewPool()
	s.SetSandboxManager(sandbox.NewManager(pool, daemonClient))

	go pool.Populate(ctx)
	handleSignals(ctx, cancel, s)

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

func (s *Server) Start(addr string) error {
	s.httpServer = &http.Server{Addr: addr, Handler: s.router}

	err := s.httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		fmt.Println("main server gracefully stopped.")
		err = nil
	}
	return err
}

func (s *Server) Cleanup() {
	s.cleanupOnce.Do(func() {
		// stop httpServer
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer shutdownCancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Printf("HTTP server Shutdown error: %v\n", err)
		} else {
			fmt.Println("HTTP server gracefully stopped.")
		}

		if err := s.sandboxManager.(*sandbox.Manager).CleanupPool(); err != nil {
			fmt.Printf("Server cleanup error: %v\n", err)
		}
		snapshot.CleanupAllViews()
		if err := s.daemonClient.Close(); err != nil {
			fmt.Printf("Containerd cleanup error: %v\n", err)
		}
	})
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("begin CreateSandbox \n")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req = sandbox.SandboxCreateRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	peerIP, err := s.sandboxManager.Create(req)
	if err != nil {
		fmt.Printf("Failed to create sandbox: %s \n", err)

		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"ip":     peerIP,
	})
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("begin DeleteSandbox \n")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sandbox.SandboxDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	err := s.sandboxManager.Delete(req)
	if err != nil {
		fmt.Printf("Failed to delete sandbox %s: %v\n", req.SandboxId, err)
		http.Error(w, "Failed to delete sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handlePauseSandbox(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("begin PauseSandbox \n")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sandbox.SandboxPauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	snapshotId, err := s.sandboxManager.Pause(req)
	if err != nil {
		fmt.Printf("Failed to pause sandbox %s: %v\n", req.SandboxId, err)
		http.Error(w, "Failed to pause sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":     "ok",
		"snapshotId": snapshotId,
	})
}

// TODO return available snapshot_id
func (s *Server) handleListSnapshot(w http.ResponseWriter, r *http.Request) {}
