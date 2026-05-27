package hostconn

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	vsockReadyPort       = 4065
	expectedAgentVersion = "0.0.2"
)

func WaitReady(ctx context.Context, sandboxID, vsockSocketPath string, retry, timeout time.Duration) (net.Conn, error) {
	logger := ulog.GetLogger()
	payload := fmt.Sprintf("I AM SANDBOX_ID:%s\n", sandboxID)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			err := fmt.Errorf("vsock signal attempts timed out after %s", timeout)
			logger.Error("vsock signal attempts timed out", ulog.F("sandboxId", sandboxID), ulog.F("timeout", timeout))
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			conn, err := net.Dial("unix", vsockSocketPath)
			if err != nil {
				logger.Debug("failed to connect to vsock socket, retrying...", ulog.F("sandboxId", sandboxID), ulog.F("error", err))
				time.Sleep(retry)
				continue
			}

			_, err = conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", vsockReadyPort)))
			if err != nil {
				logger.Debug("failed to write CONNECT command, retrying...", ulog.F("sandboxId", sandboxID), ulog.F("error", err))
				conn.Close()
				time.Sleep(retry)
				continue
			}

			_, err = conn.Write([]byte(payload))
			if err != nil {
				logger.Warn("failed to send payload, retrying...", ulog.F("sandboxId", sandboxID), ulog.F("error", err))
				conn.Close()
				time.Sleep(retry)
				continue
			}

			logger.Debug("payload sent, waiting for OK receipt", ulog.F("sandboxId", sandboxID))
			conn.SetReadDeadline(time.Now().Add(1 * time.Second))

			respBuf := make([]byte, 64)
			n, readErr := conn.Read(respBuf)
			if readErr != nil {
				logger.Debug("failed to read receipt, retrying...", ulog.F("sandboxId", sandboxID), ulog.F("error", readErr))
				conn.Close()
				time.Sleep(retry)
				continue
			}

			vmmMsg := string(respBuf[:n])
			if !strings.Contains(vmmMsg, "OK") {
				logger.Debug("Unexpected response from VMM proxy", ulog.F("msg", vmmMsg))
				conn.Close()
				time.Sleep(retry)
				continue
			}

			conn.SetReadDeadline(time.Now().Add(2 * time.Second))

			readyBuf := make([]byte, 64)
			rn, rerr := conn.Read(readyBuf)
			if rerr != nil {
				logger.Debug("Waiting for Agent READY signal timed out", ulog.F("error", rerr))
				conn.Close()
				time.Sleep(retry)
				continue
			}

			agentMsg := string(readyBuf[:rn])

			if strings.Contains(agentMsg, "NOT_READY") {
				logger.Error("Agent gRPC service not started", ulog.F("sandboxId", sandboxID))
				conn.Close()
				time.Sleep(retry)
				continue
			}

			if strings.Contains(agentMsg, "READY:") {
				parts := strings.SplitN(agentMsg, "READY:", 2)
				agentVersion := ""
				if len(parts) > 1 {
					agentVersion = strings.TrimSpace(parts[1])
				}

				if agentVersion != expectedAgentVersion {
					logger.Warn("Received agent signal but version mismatch",
						ulog.F("sandboxId", sandboxID),
						ulog.F("agent_version", agentVersion),
						ulog.F("expected_version", expectedAgentVersion))
				} else {
					logger.Info("Sandbox Agent is officially READY!",
						ulog.F("sandboxId", sandboxID),
						ulog.F("agent_version", agentVersion))
				}

				conn.SetReadDeadline(time.Time{})
				return conn, nil
			}
			logger.Warn("Received unknown message from Agent", ulog.F("msg", agentMsg))
			conn.Close()
			time.Sleep(retry)
		}
	}
}
