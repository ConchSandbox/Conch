package hostconn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/pkg/ulog"
	"golang.org/x/sys/unix"
)

func TestWaitForVsockAgentReadyReportsTimeout(t *testing.T) {
	errCh := make(chan error, 1)
	go func() {
		err := WaitReady(
			context.Background(),
			ReadyOptions{
				SandboxID:       "sandbox-timeout",
				AgentToken:      "token",
				Network:         testGuestNetwork(),
				VsockSocketPath: t.TempDir() + "/missing.vsock",
				Retry:           time.Millisecond,
				Timeout:         10 * time.Millisecond,
			},
		)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if !strings.Contains(err.Error(), "vsock signal attempts timed out") {
			t.Fatalf("error = %q, want timeout", err.Error())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ready result")
	}
}

func TestWaitReadyReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := WaitReady(
		ctx,
		ReadyOptions{
			SandboxID:       "sandbox-canceled",
			AgentToken:      "token",
			Network:         testGuestNetwork(),
			VsockSocketPath: t.TempDir() + "/missing.vsock",
			Retry:           time.Millisecond,
			Timeout:         time.Second,
		},
	)
	if err == nil || err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestWaitReadySendsEnvironmentAndNetwork(t *testing.T) {
	socketPath := t.TempDir() + "/vsock.sock"
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	requestCh := make(chan agentprotocol.InitRequest, 1)
	serverErrCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrCh <- err
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		command, err := reader.ReadString('\n')
		if err != nil {
			serverErrCh <- err
			return
		}
		if command != fmt.Sprintf("CONNECT %d\n", vsockReadyPort) {
			serverErrCh <- fmt.Errorf("unexpected proxy command %q", command)
			return
		}
		if _, err := conn.Write([]byte("OK\n")); err != nil {
			serverErrCh <- err
			return
		}

		var request agentprotocol.InitRequest
		if err := agentprotocol.ReadFrame(reader, &request); err != nil {
			serverErrCh <- err
			return
		}
		requestCh <- request
		if err := agentprotocol.WriteFrame(conn, agentprotocol.ReadyResponse()); err != nil {
			serverErrCh <- err
			return
		}
		buf := make([]byte, 1)
		if _, err := conn.Read(buf); !errors.Is(err, io.EOF) {
			serverErrCh <- fmt.Errorf("wait for client close: %w", err)
			return
		}
		serverErrCh <- nil
	}()

	err = WaitReady(context.Background(), ReadyOptions{
		SandboxID:       "sandbox-1",
		AgentToken:      "token",
		Env:             map[string]string{"SOME_RANDOM_KEY": "key123"},
		Network:         testGuestNetwork(),
		VsockSocketPath: socketPath,
		Retry:           time.Millisecond,
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if err := <-serverErrCh; err != nil {
		t.Fatalf("serve initialization request: %v", err)
	}

	request := <-requestCh
	if request.Version != agentprotocol.ProtocolVersion || request.Env["SOME_RANDOM_KEY"] != "key123" {
		t.Fatalf("initialization request = %#v", request)
	}
	if request.Network.GuestIP != "192.168.100.21" || request.Network.PrefixLength != 24 {
		t.Fatalf("network = %#v", request.Network)
	}
}

func TestValidateReadyPreflightDoesNotRequireNetwork(t *testing.T) {
	err := ValidateReadyPreflight(ReadyOptions{
		SandboxID:  "sandbox-1",
		AgentToken: "token",
		Env:        map[string]string{"KEY": "value"},
	})
	if err != nil {
		t.Fatalf("ValidateReadyPreflight() error = %v", err)
	}
}

func TestWaitReadyRejectsInvalidNetwork(t *testing.T) {
	invalid := testGuestNetwork()
	invalid.Gateway = ""
	err := WaitReady(context.Background(), ReadyOptions{
		SandboxID:       "sandbox-1",
		AgentToken:      "token",
		Network:         invalid,
		VsockSocketPath: t.TempDir() + "/missing.vsock",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid guest network config") {
		t.Fatalf("WaitReady() error = %v, want invalid network error", err)
	}
}

func TestWaitReadyRejectsPayloadLargerThanLimit(t *testing.T) {
	err := WaitReady(context.Background(), ReadyOptions{
		SandboxID:       "sandbox-1",
		AgentToken:      "token",
		Env:             map[string]string{"TOO_LARGE": strings.Repeat("x", agentprotocol.MaxPayloadSize)},
		Network:         testGuestNetwork(),
		VsockSocketPath: t.TempDir() + "/missing.vsock",
	})
	if err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("WaitReady() error = %v, want payload limit error", err)
	}
}

func TestExchangeInitHandlesReadyAndTerminalResponses(t *testing.T) {
	for _, tt := range []struct {
		name     string
		response agentprotocol.InitResponse
		wantErr  bool
		terminal bool
	}{
		{name: "ready", response: agentprotocol.ReadyResponse()},
		{name: "retryable", response: agentprotocol.NotReadyResponse("SERVICES_STARTING", "wait", true), wantErr: true},
		{name: "terminal", response: agentprotocol.NotReadyResponse("NETWORK_MISMATCH", "bad", false), wantErr: true, terminal: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			go func() {
				defer server.Close()
				var request agentprotocol.InitRequest
				if err := agentprotocol.ReadFrame(server, &request); err != nil {
					return
				}
				_ = agentprotocol.WriteFrame(server, tt.response)
			}()
			err := exchangeInit(client, agentprotocol.InitRequest{Version: agentprotocol.ProtocolVersion}, "sandbox-1", ulog.GetLogger())
			if (err != nil) != tt.wantErr {
				t.Fatalf("exchangeInit() error = %v, wantErr=%v", err, tt.wantErr)
			}
			if errors.Is(err, errInitRejected) != tt.terminal {
				t.Fatalf("terminal error = %v, want %v", errors.Is(err, errInitRejected), tt.terminal)
			}
		})
	}
}

func testGuestNetwork() netstack.GuestNetworkConfig {
	return netstack.GuestNetworkConfig{
		GuestIP:      "192.168.100.21",
		PrefixLength: 24,
		Gateway:      "192.168.100.2",
		DNS:          netstack.DNSConfig{Nameservers: []string{"10.0.0.53"}},
	}
}

func TestIsVsockUnsupported(t *testing.T) {
	if !isVsockUnsupported(unix.EAFNOSUPPORT) {
		t.Fatal("EAFNOSUPPORT should be unsupported")
	}
	if isVsockUnsupported(unix.ENODEV) {
		t.Fatal("ENODEV should stay retryable")
	}
	wrapped := fmt.Errorf("%w: %w", errVsockUnsupported, unix.EAFNOSUPPORT)
	if !errors.Is(wrapped, errVsockUnsupported) {
		t.Fatal("wrapped errVsockUnsupported should match errors.Is")
	}
}
