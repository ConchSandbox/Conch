package hostconn

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/pkg/ulog"
)

func TestWaitReadyFallsBackForLegacyStrictDecoder(t *testing.T) {
	var calls int
	dial := func(ReadyOptions, ulog.Logger) (initConn, error) {
		calls++
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			// This type reproduces the previous guest's strict wire contract.
			var legacy struct {
				Version    int               `json:"version"`
				SandboxID  string            `json:"sandboxID"`
				AgentToken string            `json:"agentToken"`
				Env        map[string]string `json:"env,omitempty"`
				Network    json.RawMessage   `json:"network"`
			}
			err := agentprotocol.ReadFrame(server, &legacy)
			if err == nil {
				_ = agentprotocol.WriteFrame(server, agentprotocol.ReadyResponse())
			}
		}()
		return client, nil
	}
	err := waitReady(context.Background(), ReadyOptions{SandboxID: "sandbox-a", Timeout: time.Second, Retry: time.Millisecond},
		agentprotocol.InitRequest{Version: agentprotocol.ProtocolVersion, RequestID: "legacy-request-206"}, ulog.GetLogger(), dial)
	if err != nil || calls != 2 {
		t.Fatalf("legacy startup: calls %d, error %v", calls, err)
	}
}

func TestWaitReadyDoesNotDowngradeTerminalRejection(t *testing.T) {
	var calls int
	dial := func(ReadyOptions, ulog.Logger) (initConn, error) {
		calls++
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var req agentprotocol.InitRequest
			if err := agentprotocol.ReadFrame(server, &req); err == nil {
				_ = agentprotocol.WriteFrame(server, agentprotocol.NotReadyResponse("invalid network", false))
			}
		}()
		return client, nil
	}
	err := waitReady(context.Background(), ReadyOptions{Timeout: time.Second, Retry: time.Millisecond},
		agentprotocol.InitRequest{Version: agentprotocol.ProtocolVersion, RequestID: "rejected-request-206"}, ulog.GetLogger(), dial)
	if !errors.Is(err, errInitRejected) || calls != 1 {
		t.Fatalf("terminal response retried: calls %d, error %v", calls, err)
	}
}
