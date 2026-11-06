package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/openeuler/Conch/internal/cluster"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/e2bapi"
	"github.com/openeuler/Conch/internal/envd"
	conchsandbox "github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandboxproxy"
	"github.com/openeuler/Conch/internal/util"
	"github.com/openeuler/Conch/pkg/ulog"
)

// initE2B wires the E2B node entrypoints onto the sandbox Manager: it owns the
// route slot, the capacity reservation and the envd handshake for every E2B
// request under the sandbox lock. A nil manager skips the injection for
// listener-only wiring.
func (s *Daemon) initE2B(cfg *config.Config, manager *conchsandbox.Manager) error {
	if cfg.E2B.ListenAddr == "" {
		return nil
	}
	capacity, err := conchruntime.NewCapacity(conchruntime.CapacityLimits{})
	if err != nil {
		return err
	}
	s.runtimeService.Store = s.sandboxStore
	s.proxyRoutes = sandboxproxy.NewRegistry()
	if manager != nil {
		manager.Routes = s.proxyRoutes
		manager.Capacity = capacity
		manager.Envd = envd.NewClient()
	}
	handler, err := e2bapi.New(e2bapi.Config{
		APIKey: cfg.E2B.APIKey, Domains: cfg.E2B.SandboxProxyDomains, RequestTimeout: cfg.Sandbox.RequestTimeout,
		MaxSandboxTTL: cfg.E2B.MaxSandboxTTL,
	}, s.runtimeService, s.proxyRoutes)
	if err != nil {
		return err
	}
	s.e2bHandler = handler
	s.e2bServer = newHTTPServer(handler)
	s.e2bListenAddr = cfg.E2B.ListenAddr
	if cfg.Cluster.SchedulerAddr != "" {
		s.reporter, err = cluster.NewReporter(cluster.Config{
			SchedulerAddr: cfg.Cluster.SchedulerAddr, NodeID: cfg.Cluster.NodeID, ClusterID: cfg.Cluster.ClusterID,
			Interval: 5 * time.Second, RPCTimeout: 10 * time.Second,
		}, s.sandboxStore, s.runtimeService)
		if err != nil {
			return err
		}
	}
	return nil
}

// serveE2B binds both Node entrypoints before starting reporting. Background
// workers and listeners have the same lifetime and are stopped before teardown.
func (s *Daemon) serveE2B(unixListener net.Listener) error {
	s.e2bLifecycleMu.Lock()
	if s.e2bStopping {
		s.e2bLifecycleMu.Unlock()
		_ = unixListener.Close()
		return nil
	}
	tcpListener, err := net.Listen("tcp", s.e2bListenAddr)
	if err != nil {
		s.e2bLifecycleMu.Unlock()
		_ = unixListener.Close()
		return fmt.Errorf("listen on E2B Node API %s: %w", s.e2bListenAddr, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.e2bCancel = cancel
	if s.reporter != nil {
		if err := s.reporter.Start(ctx); err != nil {
			cancel()
			_ = tcpListener.Close()
			_ = unixListener.Close()
			s.e2bLifecycleMu.Unlock()
			return err
		}
	}
	// Keep the lifecycle handle separate from the wrapped HTTP handler.
	handler := s.e2bHandler
	s.e2bWorkers.Add(1)
	go func() { defer s.e2bWorkers.Done(); handler.RunExpiry(ctx) }()
	results := make(chan error, 2)
	go func() { results <- s.e2bServer.Serve(tcpListener) }()
	go func() { results <- s.httpServer.Serve(unixListener) }()
	s.e2bLifecycleMu.Unlock()
	ulog.Info("E2B Node API listening", ulog.F("address", s.e2bListenAddr))
	util.NotifyReady()
	first := <-results
	s.Shutdown()
	second := <-results
	if errors.Is(first, http.ErrServerClosed) {
		first = nil
	}
	if errors.Is(second, http.ErrServerClosed) {
		second = nil
	}
	return errors.Join(first, second)
}

func (s *Daemon) shutdownE2B() {
	s.e2bLifecycleMu.Lock()
	s.e2bStopping = true
	if s.e2bCancel != nil {
		s.e2bCancel()
	}
	s.e2bLifecycleMu.Unlock()
	if s.e2bServer == nil {
		return
	}
	if s.proxyRoutes != nil {
		s.proxyRoutes.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.e2bServer.Shutdown(ctx); err != nil {
		ulog.Warn("E2B HTTP shutdown", ulog.F("error", err))
		_ = s.e2bServer.Close()
	}
	s.e2bWorkers.Wait()
	if s.reporter != nil {
		if err := s.reporter.Shutdown(ctx); err != nil {
			ulog.Warn("Scheduler unregister", ulog.F("error", err))
		}
	}
}
