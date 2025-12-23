package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/unix"

	"conch/internal/sandbox"
	"conch/internal/sandbox/network"
	"conch/internal/snapshot"
)

const (
	workDir = "/tmp/snapshot"
)

type Server struct {
	router         *http.ServeMux
	sandboxManager sandboxManager

	// TODO: need ListCachedBuilds()
}

type sandboxManager interface {
	Create(req sandbox.SandboxCreateRequest) (string, error)
	Delete(req sandbox.SandboxDeleteRequest) error
	Pause(req sandbox.SandboxPauseRequest) error

	RunCode(code string) map[string]interface{}
}

func handleSignals(ctx context.Context, cancel context.CancelFunc, pool *network.Pool) {
	go func() {
		var s os.Signal
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
			case s = <-signalChannel:
				fmt.Printf("interrupted by a %v signal, process exiting\n", s)
				cancel()
				err := pool.Cleanup()
				if err != nil {
					fmt.Printf("Failed to cleanup pool: %v\n", err)
				}
				fmt.Println("cleanup pool finish")
				return
			}
		}
	}()
	return
}

func NewServer() *Server {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		router: http.NewServeMux(),
	}
	s.routes()

	pool := network.NewPool()
	go pool.Populate(ctx)

	s.SetSandboxManager(sandbox.NewManager(pool))
	err := s.SetSnapshotManager()
	if err != nil {
		fmt.Printf("Failed to init snapshot manager: %v", err)
		// TODO return err
	}

	handleSignals(ctx, cancel, pool)
	return s
}

func (s *Server) SetSandboxManager(manager sandboxManager) {
	s.sandboxManager = manager
}

func (s *Server) SetSnapshotManager() error {
	err := snapshot.NewServer(workDir)
	if err != nil {
		fmt.Printf("init server with: %s, get err: %v", workDir, err)

		return err
	}
	return nil
}

func (s *Server) routes() {
	// sandbox
	s.router.HandleFunc("/api/sandbox/create", s.handleCreateSandbox)
	s.router.HandleFunc("/api/sandbox/delete", s.handleDeleteSandbox)
	s.router.HandleFunc("/api/sandbox/pause", s.handlePauseSandbox)
	s.router.HandleFunc("/api/sandbox/run", s.handleRunCode)

	s.router.HandleFunc("/api/snapshot/list", s.handleListSnapshot)

	s.router.HandleFunc("/health", s.handleHealthCheck)
}

func (s *Server) Start(addr string) error {
	fmt.Printf("Server listening on %s\n", addr)
	return http.ListenAndServe(addr, s.router)
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	// debug
	fmt.Printf("begin CreateSandbox *** %d ***\n", time.Now().UnixNano()/int64(time.Millisecond))

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req = sandbox.SandboxCreateRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	slotKey, err := s.sandboxManager.Create(req)
	if err != nil {
		// debug
		fmt.Printf("Failed to create sandbox: %s \n", err)

		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"slotkey": slotKey,
	})
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	// debug
	fmt.Printf("begin DeleteSandbox *** %d ***\n", time.Now().UnixNano()/int64(time.Millisecond))

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
	// debug
	fmt.Printf("begin PauseSandbox *** %d ***\n", time.Now().UnixNano()/int64(time.Millisecond))

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sandbox.SandboxPauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	err := s.sandboxManager.Pause(req)
	if err != nil {
		fmt.Printf("Failed to pause sandbox %s: %v\n", req.SandboxId, err)
		http.Error(w, "Failed to pause sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleListSnapshot(w http.ResponseWriter, r *http.Request) {}

func (s *Server) handleRunCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Code string `json:"code"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	execution := s.sandboxManager.RunCode(req.Code)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(execution)
}

func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}