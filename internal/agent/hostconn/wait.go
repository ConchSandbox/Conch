package hostconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	vsockReadyPort    = 4065
	vsockReadTimeout  = 2 * time.Second
	stratovirtVMMName = "stratovirt"
)

type ReadyOptions struct {
	SandboxID       string
	AgentToken      string
	Env             map[string]string
	Network         netstack.GuestNetworkConfig
	VMMName         string
	VsockCID        uint32
	VsockSocketPath string
	Retry           time.Duration
	Timeout         time.Duration
}

type initConn interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

func WaitReady(ctx context.Context, opts ReadyOptions) error {
	logger := ulog.GetLogger()
	if err := ValidateReadyPreflight(opts); err != nil {
		return err
	}
	if err := opts.Network.Validate(); err != nil {
		return fmt.Errorf("invalid guest network config: %w", err)
	}
	request := agentprotocol.InitRequest{
		Version:    agentprotocol.ProtocolVersion,
		SandboxID:  opts.SandboxID,
		AgentToken: opts.AgentToken,
		Env:        opts.Env,
		Network:    opts.Network.Clone(),
	}
	if _, err := agentprotocol.MarshalPayload(request); err != nil {
		return fmt.Errorf("marshal vsock initialization request: %w", err)
	}

	if opts.Retry <= 0 {
		opts.Retry = 10 * time.Millisecond
	}

	dial := dialUnixProxy
	if opts.VMMName == stratovirtVMMName {
		dial = dialVhostVsock
	}
	return waitReady(ctx, opts, request, logger, dial)
}

// ValidateReadyPreflight checks initialization data available before sandbox creation.
func ValidateReadyPreflight(opts ReadyOptions) error {
	if opts.SandboxID == "" {
		return fmt.Errorf("sandbox ID is required")
	}
	if opts.AgentToken == "" {
		return fmt.Errorf("agent token is required")
	}
	if _, err := json.Marshal(opts.Env); err != nil {
		return fmt.Errorf("marshal sandbox environment: %w", err)
	}
	return nil
}

func waitReady(ctx context.Context, opts ReadyOptions, request agentprotocol.InitRequest, logger ulog.Logger, dial func(ReadyOptions, ulog.Logger) (initConn, error)) error {
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
			return fmt.Errorf("vsock signal attempts timed out after %s (last error: %v)", opts.Timeout, lastErr)
		case <-ctx.Done():
			return ctx.Err()
		default:
			conn, err := dial(opts, logger)
			if err == nil {
				err = exchangeInit(conn, request, opts.SandboxID, logger)
				closeErr := conn.Close()
				if err == nil {
					if closeErr != nil {
						logger.Warn("failed to close vsock initialization connection", ulog.F("sandboxId", opts.SandboxID), ulog.F("error", closeErr))
					}
					return nil
				}
			}
			if errors.Is(err, errVsockUnsupported) {
				logger.Error("AF_VSOCK is unsupported or not permitted on this host; aborting sandbox startup",
					ulog.F("sandboxId", opts.SandboxID),
					ulog.F("error", err))
				return err
			}
			if errors.Is(err, errInitRejected) {
				return err
			}
			lastErr = err
			time.Sleep(opts.Retry)
		}
	}
}

func dialUnixProxy(opts ReadyOptions, logger ulog.Logger) (initConn, error) {
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

	_ = conn.SetReadDeadline(time.Time{})
	closeConn = false
	return conn, nil
}

type vhostVsockConn struct {
	*os.File
}

func (c *vhostVsockConn) SetReadDeadline(deadline time.Time) error {
	return setSocketDeadline(int(c.Fd()), unix.SO_RCVTIMEO, deadline)
}

func (c *vhostVsockConn) SetWriteDeadline(deadline time.Time) error {
	return setSocketDeadline(int(c.Fd()), unix.SO_SNDTIMEO, deadline)
}

func setSocketDeadline(fd int, option int, deadline time.Time) error {
	var timeout time.Duration
	if !deadline.IsZero() {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			timeout = time.Microsecond
		}
	}
	tv := unix.NsecToTimeval(int64(timeout))
	return unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, option, &tv)
}

func dialVhostVsock(opts ReadyOptions, logger ulog.Logger) (initConn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		if isVsockUnsupported(err) {
			return nil, fmt.Errorf("%w: %w", errVsockUnsupported, err)
		}
		logger.Debug("failed to create AF_VSOCK socket for Stratovirt, retrying...", ulog.F("sandboxId", opts.SandboxID), ulog.F("error", err))
		return nil, err
	}

	sa := &unix.SockaddrVM{CID: opts.VsockCID, Port: vsockReadyPort}
	if err = unix.Connect(fd, sa); err != nil {
		_ = unix.Close(fd)
		if isVsockUnsupported(err) {
			return nil, fmt.Errorf("%w: %w", errVsockUnsupported, err)
		}
		if !errors.Is(err, unix.ECONNRESET) && !errors.Is(err, unix.ENODEV) {
			logger.Debug("failed to connect to VM vsock for Stratovirt, retrying...",
				ulog.F("sandboxId", opts.SandboxID),
				ulog.F("cid", opts.VsockCID),
				ulog.F("port", vsockReadyPort),
				ulog.F("error", err))
		}
		return nil, err
	}

	tv := unix.NsecToTimeval(int64(vsockReadTimeout))
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		logger.Debug("failed to set AF_VSOCK read timeout for Stratovirt, retrying...", ulog.F("sandboxId", opts.SandboxID), ulog.F("error", err))
		return nil, err
	}

	logger.Debug("Connected to Stratovirt VM via AF_VSOCK",
		ulog.F("sandboxId", opts.SandboxID),
		ulog.F("cid", opts.VsockCID),
		ulog.F("port", vsockReadyPort))
	file := os.NewFile(uintptr(fd), "vsock")
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap vsock fd")
	}
	return &vhostVsockConn{File: file}, nil
}

func exchangeInit(conn initConn, request agentprotocol.InitRequest, sandboxID string, logger ulog.Logger) error {
	_ = conn.SetWriteDeadline(time.Now().Add(vsockReadTimeout))
	if err := agentprotocol.WriteFrame(conn, request); err != nil {
		return fmt.Errorf("write initialization request: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	_ = conn.SetReadDeadline(time.Now().Add(vsockReadTimeout))
	var response agentprotocol.InitResponse
	if err := agentprotocol.ReadFrame(conn, &response); err != nil {
		return fmt.Errorf("read initialization response: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if response.Version != agentprotocol.ProtocolVersion {
		return fmt.Errorf("agent protocol version %d, want %d", response.Version, agentprotocol.ProtocolVersion)
	}
	if response.Status != "ready" {
		err := fmt.Errorf("agent reported %s: %s", response.ErrorCode, response.Message)
		if !response.Retryable {
			return fmt.Errorf("%w: %v", errInitRejected, err)
		}
		return err
	}
	logger.Info("Sandbox Agent is officially READY!",
		ulog.F("sandboxId", sandboxID))
	return nil
}

var errVsockUnsupported = errors.New("AF_VSOCK unsupported on host")
var errInitRejected = errors.New("agent initialization rejected")

func isVsockUnsupported(err error) bool {
	return errors.Is(err, unix.EAFNOSUPPORT) ||
		errors.Is(err, unix.EPROTONOSUPPORT) ||
		errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES)
}
