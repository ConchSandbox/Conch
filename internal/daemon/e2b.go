package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/openeuler/Conch/internal/adapters/bolt/volumestore"
	"github.com/openeuler/Conch/internal/cluster"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/e2bapi"
	"github.com/openeuler/Conch/internal/envd"
	"github.com/openeuler/Conch/internal/sandboxproxy"
	"github.com/openeuler/Conch/internal/util"
	"github.com/openeuler/Conch/internal/volume"
	"github.com/openeuler/Conch/pkg/ulog"
)

func (s *Daemon) initE2B(cfg *config.Config) error {
	if cfg.E2B.ListenAddr == "" {
		return nil
	}
	capacity, err := conchruntime.NewCapacity(conchruntime.CapacityLimits{
		MaxSandboxes: 250, // Internal Node ceiling; CPU/RAM budgets use host capacity.
	})
	if err != nil {
		return err
	}
	s.runtimeService.Capacity = capacity
	s.runtimeService.Store = s.sandboxStore
	s.runtimeService.Envd = envd.NewClient()
	s.runtimeService.ProxyRoutes = sandboxproxy.NewRegistry()
	volumeStore, err := volumestore.Open(cfg.VolumeDBPath())
	if err != nil {
		return fmt.Errorf("open volume registry: %w", err)
	}
	volumes, err := volume.NewRegistry(volumeStore, cfg.VolumeDataDir())
	if err != nil {
		_ = volumeStore.Close()
		return fmt.Errorf("init volume registry: %w", err)
	}
	handler, err := e2bapi.New(e2bapi.Config{
		APIKey: cfg.E2B.APIKey, Domains: cfg.E2B.SandboxProxyDomains, RequestTimeout: cfg.Sandbox.RequestTimeout,
	}, s.runtimeService, s.runtimeService.ProxyRoutes)
	if err != nil {
		_ = volumeStore.Close()
		return err
	}
	handler.SetVolumes(volumes)
	s.volumeStore = volumeStore
	s.e2bServer = newHTTPServer(handler)
	s.e2bListenAddr = cfg.E2B.ListenAddr
	if cfg.Cluster.SchedulerAddr != "" {
		s.reporter, err = cluster.NewReporter(cluster.Config{
			SchedulerAddr: cfg.Cluster.SchedulerAddr, NodeID: cfg.Cluster.NodeID, ClusterID: cfg.Cluster.ClusterID,
			Interval: 5 * time.Second, RPCTimeout: 5 * time.Second,
		}, s.sandboxStore, s.runtimeService)
		if err != nil {
			_ = volumeStore.Close()
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
	handler := s.e2bServer.Handler.(*e2bapi.Server)
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
	if s.runtimeService != nil && s.runtimeService.ProxyRoutes != nil {
		s.runtimeService.ProxyRoutes.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.e2bServer.Shutdown(ctx); err != nil {
		ulog.Warn("E2B HTTP shutdown", ulog.F("error", err))
		_ = s.e2bServer.Close()
	}
	s.e2bWorkers.Wait()
	if s.volumeStore != nil {
		if err := s.volumeStore.Close(); err != nil {
			ulog.Warn("close volume registry", ulog.F("error", err))
		}
		s.volumeStore = nil
	}
	if s.reporter != nil {
		if err := s.reporter.Shutdown(ctx); err != nil {
			ulog.Warn("Scheduler unregister", ulog.F("error", err))
		}
	}
}
