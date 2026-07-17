package hostconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	vsockReadyPort       = 4065
	expectedAgentVersion = "0.0.4"
	vsockReadTimeout     = 2 * time.Second
	stratovirtVMMName    = "stratovirt"
)

type ReadyOptions struct {
	SandboxID       string
	AgentToken      string
	Env             map[string]string
	VMMName         string
	VsockCID        uint32
	VsockSocketPath string
	Retry           time.Duration
	Timeout         time.Duration
}

func WaitReady(ctx context.Context, opts ReadyOptions) (net.Conn, error) {
	logger := ulog.GetLogger()
	payload, err := readyPayload(opts)
	if err != nil {
		return nil, err
	}

	if opts.Retry <= 0 {
		opts.Retry = 10 * time.Millisecond
	}

	if opts.VMMName == stratovirtVMMName {
		return waitReadyVhostVsock(ctx, opts, payload, logger)
	}
	return waitReadyUnixProxy(ctx, opts, payload, logger)
}

func readyPayload(opts ReadyOptions) (string, error) {
	payload := fmt.Sprintf("I AM SANDBOX_ID:%s\nAGENT_TOKEN:%s\n", opts.SandboxID, opts.AgentToken)
	if len(opts.Env) == 0 {
		return payload, nil
	}
	env, err := json.Marshal(opts.Env)
	if err != nil {
		return "", fmt.Errorf("marshal sandbox environment: %w", err)
	}
	return payload + "ENV_JSON:" + string(env) + "\n", nil
}

func waitReadyUnixProxy(ctx context.Context, opts ReadyOptions, payload string, logger ulog.Logger) (net.Conn, error) {
	timer := time.NewTimer(opts.Timeout)
	defer timer.Stop()

	var lastErr error
	for {
		select {
		case <-timer.C:
			logger.Error("vsock signal attempts timed out",
				ulog.F("sandboxId", opts.SandboxID),
				ulog.F("timeout", opts.Timeout),
				ulog.F("last_error", lastErr))
			return nil, fmt.Errorf("vsock signal attempts timed out after %s (last error: %v)", opts.Timeout, lastErr)
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			conn, err := tryUnixProxyReady(opts, payload, logger)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			time.Sleep(opts.Retry)
		}
	}
}

func tryUnixProxyReady(opts ReadyOptions, payload string, logger ulog.Logger) (net.Conn, error) {
	conn, err := net.Dial("unix", opts.VsockSocketPath)
	if err != nil {
		return nil, err
	}

	closeConn := true
	defer func() {
		if closeConn {
			_ = conn.Close()
		}
	}()

	if _, err = conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", vsockReadyPort))); err != nil {
		logger.Debug("failed to write CONNECT command, retrying...", ulog.F("sandboxId", opts.SandboxID), ulog.F("error", err))
		return nil, err
	}

	if _, err = conn.Write([]byte(payload)); err != nil {
		logger.Warn("failed to send payload, retrying...", ulog.F("sandboxId", opts.SandboxID), ulog.F("error", err))
		return nil, err
	}

	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	respBuf := make([]byte, 64)
	n, err := conn.Read(respBuf)
	if err != nil {
		return nil, err
	}
	vmmMsg := string(respBuf[:n])
	if !strings.Contains(vmmMsg, "OK") {
		err := fmt.Errorf("unexpected response from VMM proxy: %q", vmmMsg)
		logger.Debug("Unexpected response from VMM proxy", ulog.F("msg", vmmMsg))
		return nil, err
	}

	_ = conn.SetReadDeadline(time.Now().Add(vsockReadTimeout))
	if err := readAgentReadyFromConn(conn, opts.SandboxID, logger); err != nil {
		return nil, err
	}

	_ = conn.SetReadDeadline(time.Time{})
	closeConn = false
	return conn, nil
}

func waitReadyVhostVsock(ctx context.Context, opts ReadyOptions, payload string, logger ulog.Logger) (net.Conn, error) {
	timer := time.NewTimer(opts.Timeout)
	defer timer.Stop()

	var lastErr error
	for {
		select {
		case <-timer.C:
			logger.Error("vsock signal attempts timed out for Stratovirt",
				ulog.F("sandboxId", opts.SandboxID),
				ulog.F("timeout", opts.Timeout),
				ulog.F("last_error", lastErr))
			return nil, fmt.Errorf("vsock signal attempts timed out after %s (last error: %v)", opts.Timeout, lastErr)
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			conn, err := tryVhostVsockReady(opts, payload, logger)
			if err == nil {
				return conn, nil
			}
			if errors.Is(err, errVsockUnsupported) {
				logger.Error("AF_VSOCK is unsupported or not permitted on this host; aborting sandbox startup",
					ulog.F("sandboxId", opts.SandboxID),
					ulog.F("error", err))
				return nil, err
			}
			lastErr = err
			time.Sleep(opts.Retry)
		}
	}
}

func tryVhostVsockReady(opts ReadyOptions, payload string, logger ulog.Logger) (net.Conn, error) {
	fd, err := dialVhostVsock(opts, logger)
	if err != nil {
		return nil, err
	}

	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()

	if _, err = unix.Write(fd, []byte(payload)); err != nil {
		logger.Warn("failed to send payload to Stratovirt VM, retrying...", ulog.F("sandboxId", opts.SandboxID), ulog.F("error", err))
		return nil, err
	}
	logger.Debug("payload sent to Stratovirt VM, waiting for READY signal", ulog.F("sandboxId", opts.SandboxID))

	buf := make([]byte, 64)
	n, err := unix.Read(fd, buf)
	if err != nil {
		logger.Debug("failed to read from Stratovirt vsock, retrying...", ulog.F("sandboxId", opts.SandboxID), ulog.F("error", err))
		return nil, err
	}
	if err := validateAgentReady(string(buf[:n]), opts.SandboxID, logger, "Stratovirt "); err != nil {
		return nil, err
	}

	file := os.NewFile(uintptr(fd), "vsock")
	defer file.Close()
	closeFD = false

	conn, err := net.FileConn(file)
	if err != nil {
		logger.Info("vsock fd not convertible to net.Conn, but agent is READY",
			ulog.F("sandboxId", opts.SandboxID),
			ulog.F("error", err))
		return nil, nil
	}
	return conn, nil
}

func dialVhostVsock(opts ReadyOptions, logger ulog.Logger) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		if isVsockUnsupported(err) {
			return -1, fmt.Errorf("%w: %w", errVsockUnsupported, err)
		}
		logger.Debug("failed to create AF_VSOCK socket for Stratovirt, retrying...", ulog.F("sandboxId", opts.SandboxID), ulog.F("error", err))
		return -1, err
	}

	sa := &unix.SockaddrVM{CID: opts.VsockCID, Port: vsockReadyPort}
	if err = unix.Connect(fd, sa); err != nil {
		_ = unix.Close(fd)
		if isVsockUnsupported(err) {
			return -1, fmt.Errorf("%w: %w", errVsockUnsupported, err)
		}
		if !errors.Is(err, unix.ECONNRESET) && !errors.Is(err, unix.ENODEV) {
			logger.Debug("failed to connect to VM vsock for Stratovirt, retrying...",
				ulog.F("sandboxId", opts.SandboxID),
				ulog.F("cid", opts.VsockCID),
				ulog.F("port", vsockReadyPort),
				ulog.F("error", err))
		}
		return -1, err
	}

	tv := unix.NsecToTimeval(int64(vsockReadTimeout))
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		logger.Debug("failed to set AF_VSOCK read timeout for Stratovirt, retrying...", ulog.F("sandboxId", opts.SandboxID), ulog.F("error", err))
		return -1, err
	}

	logger.Debug("Connected to Stratovirt VM via AF_VSOCK",
		ulog.F("sandboxId", opts.SandboxID),
		ulog.F("cid", opts.VsockCID),
		ulog.F("port", vsockReadyPort))
	return fd, nil
}

func readAgentReadyFromConn(conn net.Conn, sandboxID string, logger ulog.Logger) error {
	readyBuf := make([]byte, 64)
	n, err := conn.Read(readyBuf)
	if err != nil {
		logger.Debug("Waiting for Agent READY signal timed out", ulog.F("error", err))
		return err
	}
	return validateAgentReady(string(readyBuf[:n]), sandboxID, logger, "")
}

func validateAgentReady(agentMsg, sandboxID string, logger ulog.Logger, logPrefix string) error {
	if strings.Contains(agentMsg, "NOT_READY") {
		logger.Error("Agent API service not started", ulog.F("sandboxId", sandboxID))
		return fmt.Errorf("agent reported NOT_READY")
	}

	if !strings.Contains(agentMsg, "READY:") {
		logger.Warn("Received unknown message from Agent", ulog.F("msg", agentMsg))
		return fmt.Errorf("unknown agent message: %q", agentMsg)
	}

	parts := strings.SplitN(agentMsg, "READY:", 2)
	agentVersion := ""
	if len(parts) > 1 {
		agentVersion = strings.TrimSpace(parts[1])
	}
	if agentVersion != expectedAgentVersion {
		logger.Warn("Received "+logPrefix+"agent signal but version mismatch",
			ulog.F("sandboxId", sandboxID),
			ulog.F("agent_version", agentVersion),
			ulog.F("expected_version", expectedAgentVersion))
		return nil
	}

	logger.Info(logPrefix+"Sandbox Agent is officially READY!",
		ulog.F("sandboxId", sandboxID),
		ulog.F("agent_version", agentVersion))
	return nil
}

var errVsockUnsupported = errors.New("AF_VSOCK unsupported on host")

func isVsockUnsupported(err error) bool {
	return errors.Is(err, unix.EAFNOSUPPORT) ||
		errors.Is(err, unix.EPROTONOSUPPORT) ||
		errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES)
}
