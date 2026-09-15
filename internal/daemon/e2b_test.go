package daemon

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/config"
)

func TestE2BListenersStopTogether(t *testing.T) {
	// Obtain a local address without adding a listener injection seam to conchd.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	store := newMemorySandboxStore()
	s := newTestDaemon(store)
	s.runtimeService = newHandlerRuntime(&fakeSandboxOps{}, nil, store)
	s.routes()
	cfg := config.DefaultConfig()
	cfg.E2B.ListenAddr = addr
	cfg.E2B.APIKey = "node-key"
	if err := s.initE2B(cfg); err != nil {
		t.Fatal(err)
	}
	socket := tempSocket(t)
	finished := make(chan error, 1)
	go func() { finished <- s.Start(socket) }()
	t.Cleanup(s.Shutdown)
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		select {
		case err := <-finished:
			t.Fatalf("daemon exited before ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("E2B listener did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	unixClient := &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	resp, err := unixClient.Get("http://conch/api/v1/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("Unix API status=%d", resp.StatusCode)
	}
	s.Shutdown()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop both listeners")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("TCP listener remains open")
	}
	if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		conn.Close()
		t.Fatal("Unix listener remains open")
	}
}

func TestE2BStartAfterShutdownDoesNotStartWorkers(t *testing.T) {
	store := newMemorySandboxStore()
	s := newTestDaemon(store)
	s.runtimeService = newHandlerRuntime(&fakeSandboxOps{}, nil, store)
	cfg := config.DefaultConfig()
	cfg.E2B.ListenAddr = "127.0.0.1:18080"
	cfg.E2B.APIKey = "node-key"
	if err := s.initE2B(cfg); err != nil {
		t.Fatal(err)
	}
	s.Shutdown()
	if err := s.Start(tempSocket(t)); err != nil {
		t.Fatal(err)
	}
}
