package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/sandbox/network"
	"github.com/openeuler/Conch/internal/sandbox/vmm"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/pkg/ulog"
)

type Manager struct {
	sandboxes          sync.Map
	pool               *network.Pool
	daemonClient       *daemon.Client
	vsockSignalRetry   time.Duration
	vsockSignalTimeout time.Duration
	requestTimeout     time.Duration
	defaultVMM         string
	cidAllocator       *CIDAllocator
}

func NewManager(p *network.Pool, daemonClient *daemon.Client, vsockSignalRetry, vsockSignalTimeout, requestTimeout time.Duration, defaultVMM string) *Manager {
	return &Manager{
		pool:               p,
		daemonClient:       daemonClient,
		vsockSignalRetry:   vsockSignalRetry,
		vsockSignalTimeout: vsockSignalTimeout,
		requestTimeout:     requestTimeout,
		defaultVMM:         defaultVMM,
		cidAllocator:       NewCIDAllocator(),
	}
}

type SandboxCreateRequest struct {
	Namespace   string `json:"namespace"`
	SnapshotId  string `json:"snapshot_id"`
	ImageName   string `json:"image_name"`
	UseSnapshot bool   `json:"use_snapshot"`
	VmmName     string `json:"vmm_name"`
	SandboxId   string `json:"sandbox_id"`
	VcpuNum     int64  `json:"vcpu_num"`
	VcpuMax     int64  `json:"vcpu_max"`
	RamMB       int64  `json:"ram_mb"`
}

type SandboxDeleteRequest struct {
	Namespace string `json:"namespace"`
	SandboxId string `json:"sandbox_id"`
}

type SandboxPauseRequest struct {
	Namespace string `json:"namespace"`
	SandboxId string `json:"sandbox_id"`
}

const (
	vsockReadyPort       = 4065
	expectedAgentVersion = "0.0.2"
)

func sandboxMapKey(namespace, sandboxID string) string {
	return namespace + ":" + sandboxID
}

func createSandboxWithVsockSend(ctx context.Context, snapshotConf *snapshot.SnapshotConfig, namespace, vmmName, sandboxId string, vcpuNum, vcpuMax int64, pool *network.Pool, vsockSignalRetry, vsockSignalTimeout time.Duration, resume bool, vsockCID uint32, vsockSocketPath string) (*Sandbox, error) {
	logger := ulog.GetLogger()

	var sbx *Sandbox
	var createErr error
	if resume {
		sbx, createErr = ResumeSandbox(ctx, snapshotConf, namespace, vmmName, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath)
	} else {
		sbx, createErr = CreateSandbox(ctx, snapshotConf, namespace, vmmName, sandboxId, vcpuNum, vcpuMax, pool, vsockCID, vsockSocketPath)
	}
	if createErr != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", createErr)
	}

	errCh := make(chan error, 1)
	// StratoVirt exposes a real vhost-vsock device, so the host talks to the guest
	// agent over AF_VSOCK directly; cloud-hypervisor proxies vsock over a unix socket.
	// The transport difference is hidden behind the vsockAgentWaiter interface.
	vmmType, _ := vmm.GetVmmType(vmmName)
	waiter := newVsockAgentWaiter(vmmType, vsockCID, vsockSocketPath)
	go waiter.waitForAgentReady(ctx, sbx, sandboxId, vsockSignalRetry, vsockSignalTimeout, errCh)

	select {
	case err := <-errCh:
		if err != nil {
			return sbx, err
		}
		logger.Info("Vsock signal sent successfully", ulog.F("sandboxId", sandboxId))
	case <-ctx.Done():
		return sbx, ctx.Err()
	}
	return sbx, nil
}

// vsockAgentWaiter waits until the in-VM agent reports READY and stores its connection
// on the sandbox. Each VMM exposes vsock differently, so each provides an implementation.
type vsockAgentWaiter interface {
	waitForAgentReady(ctx context.Context, sbx *Sandbox, sandboxId string, vsockSignalRetry, vsockSignalTimeout time.Duration, errCh chan error)
}

// newVsockAgentWaiter returns the vsockAgentWaiter matching the VMM type.
func newVsockAgentWaiter(vmmType int, vsockCID uint32, vsockSocketPath string) vsockAgentWaiter {
	switch vmmType {
	case vmm.StratovirtVmmType:
		return &vhostVsockWaiter{vsockCID: vsockCID}
	default:
		return &unixProxyVsockWaiter{vsockSocketPath: vsockSocketPath}
	}
}

// unixProxyVsockWaiter talks to the agent over a cloud-hypervisor unix-socket vsock proxy.
type unixProxyVsockWaiter struct {
	vsockSocketPath string
}

func (w *unixProxyVsockWaiter) waitForAgentReady(ctx context.Context, sbx *Sandbox, sandboxId string, vsockSignalRetry, vsockSignalTimeout time.Duration, errCh chan error) {
	vsockSocketPath := w.vsockSocketPath
	logger := ulog.GetLogger()
	payload := fmt.Sprintf("I AM SANDBOX_ID:%s\n", sandboxId)

	timer := time.NewTimer(vsockSignalTimeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			logger.Error("vsock signal attempts timed out", ulog.F("sandboxId", sandboxId), ulog.F("timeout", vsockSignalTimeout))
			errCh <- fmt.Errorf("vsock signal timeout after %v", vsockSignalTimeout)
			return
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		default:
			conn, err := net.Dial("unix", vsockSocketPath)
			if err != nil {
				logger.Debug("failed to connect to vsock socket, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", err))
				time.Sleep(vsockSignalRetry)
				continue
			}

			_, err = conn.Write([]byte("CONNECT 4065\n"))
			if err != nil {
				logger.Debug("failed to write CONNECT command, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", err))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			_, err = conn.Write([]byte(payload))
			if err != nil {
				logger.Warn("failed to send payload, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", err))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			conn.SetReadDeadline(time.Now().Add(1 * time.Second))

			respBuf := make([]byte, 64)
			n, readErr := conn.Read(respBuf)
			if readErr != nil {
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			vmmMsg := string(respBuf[:n])
			if !strings.Contains(vmmMsg, "OK") {
				logger.Debug("Unexpected response from VMM proxy", ulog.F("msg", vmmMsg))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			conn.SetReadDeadline(time.Now().Add(2 * time.Second))

			readyBuf := make([]byte, 64)
			rn, rerr := conn.Read(readyBuf)
			if rerr != nil {
				logger.Debug("Waiting for Agent READY signal timed out", ulog.F("error", rerr))
				conn.Close()
				time.Sleep(vsockSignalRetry)
				continue
			}

			agentMsg := string(readyBuf[:rn])

			if strings.Contains(agentMsg, "NOT_READY") {
				logger.Error("Agent gRPC service not started", ulog.F("sandboxId", sandboxId))
				conn.Close()
				time.Sleep(vsockSignalRetry)
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
						ulog.F("sandboxId", sandboxId),
						ulog.F("agent_version", agentVersion),
						ulog.F("expected_version", expectedAgentVersion))
				} else {
					logger.Info("Sandbox Agent is officially READY!",
						ulog.F("sandboxId", sandboxId),
						ulog.F("agent_version", agentVersion))
				}

				conn.SetReadDeadline(time.Time{})
				sbx.vsockConn = conn
				errCh <- nil
				return
			}
			logger.Warn("Received unknown message from Agent", ulog.F("msg", agentMsg))
			conn.Close()
			time.Sleep(vsockSignalRetry)
		}
	}
}

// vhostVsockWaiter waits for the agent over a real AF_VSOCK connection (StratoVirt
// uses vhost-vsock-pci, talking to (CID, port) directly). VM/QMP readiness is not
// sufficient: sandbox startup completes only after the agent returns a valid READY.
type vhostVsockWaiter struct {
	vsockCID uint32
}

// waitForAgentReady runs the timeout/cancellation/backoff loop, retrying trySignalAgent
// until the agent signals READY, the deadline elapses, or ctx is cancelled.
func (w *vhostVsockWaiter) waitForAgentReady(ctx context.Context, sbx *Sandbox, sandboxId string, vsockSignalRetry, vsockSignalTimeout time.Duration, errCh chan error) {
	logger := ulog.GetLogger()
	payload := fmt.Sprintf("I AM SANDBOX_ID:%s\n", sandboxId)

	timer := time.NewTimer(vsockSignalTimeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			logger.Error("vsock signal attempts timed out for Stratovirt", ulog.F("sandboxId", sandboxId), ulog.F("timeout", vsockSignalTimeout))
			errCh <- fmt.Errorf("vsock signal timeout after %v", vsockSignalTimeout)
			return
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		default:
			if w.trySignalAgent(sbx, sandboxId, payload, logger) {
				errCh <- nil
				return
			}
			time.Sleep(vsockSignalRetry)
		}
	}
}

// trySignalAgent runs one connect -> send-payload -> await-READY cycle. It returns true only
// after receiving a valid READY message. Agent version mismatches are warnings, matching the
// cloud-hypervisor behavior. It owns the socket fd, closing it on every path except the one
// handed to sbx.
func (w *vhostVsockWaiter) trySignalAgent(sbx *Sandbox, sandboxId, payload string, logger ulog.Logger) bool {
	fd, err := w.dialVsock(sandboxId, logger)
	if err != nil {
		return false
	}

	if _, err := unix.Write(fd, []byte(payload)); err != nil {
		logger.Warn("failed to send payload to Stratovirt VM, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", err))
		unix.Close(fd)
		return false
	}
	logger.Debug("payload sent to Stratovirt VM, waiting for READY signal", ulog.F("sandboxId", sandboxId))

	buf := make([]byte, 64)
	n, err := unix.Read(fd, buf)
	if err != nil {
		logger.Debug("failed to read from Stratovirt vsock, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", err))
		unix.Close(fd)
		return false
	}

	agentMsg := string(buf[:n])
	if strings.TrimSpace(agentMsg) == "NOT_READY" {
		logger.Error("Agent gRPC service not started in Stratovirt VM", ulog.F("sandboxId", sandboxId))
		unix.Close(fd)
		return false
	}

	agentVersion, err := validateStratovirtReadyMessage(agentMsg)
	if err != nil {
		logger.Warn("Received invalid READY message from Stratovirt Agent",
			ulog.F("sandboxId", sandboxId),
			ulog.F("msg", agentMsg),
			ulog.F("error", err))
		unix.Close(fd)
		return false
	}
	if agentVersion != expectedAgentVersion {
		logger.Warn("Received Stratovirt agent signal but version mismatch",
			ulog.F("sandboxId", sandboxId),
			ulog.F("agent_version", agentVersion),
			ulog.F("expected_version", expectedAgentVersion))
	} else {
		logger.Info("Stratovirt Sandbox Agent is officially READY!",
			ulog.F("sandboxId", sandboxId),
			ulog.F("agent_version", agentVersion))
	}

	w.adoptReadyConn(sbx, sandboxId, fd, logger)
	return true
}

// validateStratovirtReadyMessage validates the complete response received over AF_VSOCK.
// The agent currently prefixes READY with an OK line, but a bare READY response is also
// accepted. Substring matches, additional lines, empty versions, and malformed prefixes
// are rejected. A version mismatch is handled by the caller as a warning.
func validateStratovirtReadyMessage(message string) (string, error) {
	normalized := strings.ReplaceAll(message, "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(normalized, "\n"), "\n")
	if len(lines) == 2 && lines[0] == "OK" {
		lines = lines[1:]
	}
	if len(lines) != 1 {
		return "", fmt.Errorf("unexpected response format")
	}

	const readyPrefix = "READY:"
	if !strings.HasPrefix(lines[0], readyPrefix) {
		return "", fmt.Errorf("missing READY prefix")
	}
	version := strings.TrimPrefix(lines[0], readyPrefix)
	if version == "" || strings.TrimSpace(version) != version {
		return "", fmt.Errorf("invalid agent version")
	}
	return version, nil
}

// dialVsock opens an AF_VSOCK socket and connects it to the agent's ready port. Every
// failure is retryable by the caller until the configured vsock timeout expires. In
// particular, ENODEV can occur while StratoVirt's vhost-vsock device is still initializing.
func (w *vhostVsockWaiter) dialVsock(sandboxId string, logger ulog.Logger) (fd int, err error) {
	fd, err = unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		if isVsockUnsupported(err) {
			logger.Debug("AF_VSOCK is unavailable on this system; retrying until sandbox startup timeout",
				ulog.F("sandboxId", sandboxId),
				ulog.F("error", err))
		} else {
			logger.Debug("failed to create AF_VSOCK socket for Stratovirt, retrying...", ulog.F("sandboxId", sandboxId), ulog.F("error", err))
		}
		return -1, err
	}

	sa := &unix.SockaddrVM{CID: w.vsockCID, Port: vsockReadyPort}
	if err = unix.Connect(fd, sa); err != nil {
		unix.Close(fd)
		if !errors.Is(err, unix.ECONNRESET) && !errors.Is(err, unix.ENODEV) {
			logger.Debug("failed to connect to VM vsock for Stratovirt, retrying...",
				ulog.F("sandboxId", sandboxId),
				ulog.F("cid", w.vsockCID),
				ulog.F("port", vsockReadyPort),
				ulog.F("error", err))
		}
		return -1, err
	}

	logger.Debug("Connected to Stratovirt VM via AF_VSOCK",
		ulog.F("sandboxId", sandboxId),
		ulog.F("cid", w.vsockCID),
		ulog.F("port", vsockReadyPort))
	return fd, nil
}

// adoptReadyConn stores the connection after the caller has validated the READY message.
func (w *vhostVsockWaiter) adoptReadyConn(sbx *Sandbox, sandboxId string, fd int, logger ulog.Logger) {
	file := os.NewFile(uintptr(fd), "vsock")
	defer file.Close()

	vsockConn, err := net.FileConn(file)
	if err != nil {
		logger.Info("vsock fd not convertible to net.Conn (vsock protocol not supported by Go net package), but Agent is READY - connection established via raw fd",
			ulog.F("sandboxId", sandboxId),
			ulog.F("error", err))
		return
	}
	sbx.vsockConn = vsockConn
}

// isVsockUnsupported identifies host-level AF_VSOCK support or permission failures for
// logging. It deliberately excludes ENODEV, which is transient during device initialization.
// These errors no longer bypass the mandatory Agent READY handshake.
func isVsockUnsupported(err error) bool {
	return errors.Is(err, unix.EAFNOSUPPORT) ||
		errors.Is(err, unix.EPROTONOSUPPORT) ||
		errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES)
}

func (m *Manager) Create(req SandboxCreateRequest) (string, error) {
	logger := ulog.GetLogger()
	logger.Debug("creating sandbox in manager")

	// The hypervisor is a host-side decision: fall back to the server-configured default
	// when the client did not pin a specific vmm_name.
	if req.VmmName == "" {
		req.VmmName = m.defaultVMM
		logger.Debug("vmm_name not specified, using server default", ulog.F("vmm_name", req.VmmName))
	}

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	var sbx *Sandbox
	var peerIP string
	namespace := m.resolveNamespace(req.Namespace)

	parentIDs, err := m.resolveParentSnapshotIDs(context.Background(), namespace, req)
	if err != nil {
		return "", err
	}
	resume := req.SnapshotId != "" || req.UseSnapshot
	if resume && parentIDs.Mem == "" {
		return "", fmt.Errorf("mem snapshot label not found on rootfs snapshot %s", parentIDs.Rootfs)
	}

	var key = req.SandboxId

	memOpt := func(info *snapshot.SnapshotConfig) error {
		info.MemSize = req.RamMB
		return nil
	}

	var snapshotConf *snapshot.SnapshotConfig

	vsockCID, err := m.AllocateUniqueCID(req.SandboxId)
	if err != nil {
		return "", fmt.Errorf("failed to create sandbox: CID allocation error: %v", err)
	}
	vsockSocketPath, err := SandboxVsockSocketPath(key)
	if err != nil {
		return "", fmt.Errorf("failed to create sandbox: vsock socket path error: %v", err)
	}

	vcpuMax := req.VcpuMax
	if vcpuMax == 0 {
		vcpuMax = req.VcpuNum
	}

	if resume {
		logger.Debug("creating sandbox by snapshotId")
		snapshotConf, err = snapshot.AcquireResumeWorkspace(context.Background(), namespace, key, parentIDs, vsockCID, vsockSocketPath, memOpt)
	} else {
		logger.Debug("creating sandbox by image", ulog.F("imageName", req.ImageName))
		snapshotConf, err = snapshot.Prepare(context.Background(), namespace, key, parentIDs, memOpt)
	}

	if err != nil {
		return "", fmt.Errorf("failed to prepare/acquire snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			rmErr := snapshot.Remove(context.Background(), namespace, key)
			if rmErr != nil {
				logger.Error("failed to remove snapshot", ulog.F("key", key), ulog.F("error", rmErr))
			} else {
				logger.Info("removed snapshot due to error", ulog.F("key", key))
			}
		}
	}()

	sbx, err = createSandboxWithVsockSend(ctx, snapshotConf, namespace, req.VmmName, req.SandboxId, req.VcpuNum, vcpuMax, m.pool, m.vsockSignalRetry, m.vsockSignalTimeout, resume, vsockCID, vsockSocketPath)

	if err != nil {
		if releaseErr := m.ReleaseCID(req.SandboxId); releaseErr != nil {
			logger.Warn("failed to release CID on create failure", ulog.F("sandbox_id", req.SandboxId), ulog.F("error", releaseErr))
		}
		return "", fmt.Errorf("failed to create sandbox: %w", err)
	}
	peerIP = sbx.slot.VpeerIPString()

	mapKey := sandboxMapKey(namespace, req.SandboxId)
	m.sandboxes.Store(mapKey, sbx)
	go func() {
		waitErr := sbx.Wait(ctx)
		if waitErr != nil {
			logger.Warn("failed to wait for sandbox, cleaning up", ulog.F("error", waitErr))
		}

		cleanupErr := sbx.Close(ctx)
		if cleanupErr != nil {
			logger.Warn("failed to cleanup sandbox, will remove from cache", ulog.F("error", cleanupErr))
		}

		snapshot.Remove(context.Background(), sbx.namespace, req.SandboxId)

		if releaseErr := m.ReleaseCID(req.SandboxId); releaseErr != nil {
			logger.Warn("failed to release CID", ulog.F("sandbox_id", req.SandboxId), ulog.F("error", releaseErr))
		}

		m.sandboxes.Delete(mapKey)
	}()

	logger.Debug("created sandbox in manager")

	return peerIP, nil
}

func (m *Manager) resolveParentSnapshotIDs(
	ctx context.Context,
	namespace string,
	req SandboxCreateRequest,
) (snapshot.ParentSnapshotIDs, error) {
	var rootfsSnapshotID string

	if req.SnapshotId != "" {
		rootfsSnapshotID = req.SnapshotId
	} else {
		if req.ImageName == "" {
			return snapshot.ParentSnapshotIDs{}, fmt.Errorf("imageName or snapshotID is required")
		}

		var err error
		rootfsSnapshotID, err = image.GetSnapshotID(ctx, m.daemonClient, namespace, req.ImageName)
		if err != nil {
			return snapshot.ParentSnapshotIDs{}, fmt.Errorf("failed to resolve image snapshot: %w", err)
		}
	}

	parents, err := snapshot.ResolveImageParentSnapshotIDs(namespace, rootfsSnapshotID)
	if err != nil {
		return snapshot.ParentSnapshotIDs{}, err
	}
	return parents, nil
}

func (m *Manager) resolveNamespace(namespace string) string {
	if namespace != "" {
		return namespace
	}
	return m.daemonClient.DefaultNamespace()
}

func (m *Manager) Delete(req SandboxDeleteRequest) error {
	logger := ulog.GetLogger()

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	mapKey := sandboxMapKey(namespace, req.SandboxId)
	sbxVal, exists := m.sandboxes.Load(mapKey)
	if !exists {
		return fmt.Errorf("sandbox %s not found", req.SandboxId)
	}

	sbx, ok := sbxVal.(*Sandbox)
	if !ok {
		return fmt.Errorf("invalid sandbox type for %s", req.SandboxId)
	}

	m.sandboxes.Delete(mapKey)
	go func() {
		err := sbx.Stop(ctx)
		if err != nil {
			logger.Error("sandbox stop error", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}

		if releaseErr := m.ReleaseCID(req.SandboxId); releaseErr != nil {
			logger.Warn("failed to release CID", ulog.F("sandbox_id", req.SandboxId), ulog.F("error", releaseErr))
		}
	}()
	return nil
}

func (m *Manager) Pause(req SandboxPauseRequest) (string, error) {
	logger := ulog.GetLogger()

	ctx, cancel := context.WithTimeoutCause(context.Background(), m.requestTimeout, fmt.Errorf("request timed out"))
	defer cancel()

	namespace := m.resolveNamespace(req.Namespace)
	sbxVal, exists := m.sandboxes.Load(sandboxMapKey(namespace, req.SandboxId))
	if !exists {
		return "", fmt.Errorf("sandbox %s not found", req.SandboxId)
	}

	sbx, ok := sbxVal.(*Sandbox)
	if !ok {
		return "", fmt.Errorf("invalid sandbox type for %s", req.SandboxId)
	}

	m.sandboxes.Delete(sandboxMapKey(sbx.namespace, req.SandboxId))
	defer func() {
		logger.Info("sandbox stop in pause", ulog.F("sandboxId", req.SandboxId))
		if err := sbx.Stop(ctx); err != nil {
			logger.Error("sandbox stop error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
		if err := sbx.Close(ctx); err != nil {
			logger.Error("sandbox close error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
		if err := snapshot.Remove(context.Background(), sbx.namespace, req.SandboxId); err != nil {
			logger.Error("sandbox remove error after pause", ulog.F("sandboxId", req.SandboxId), ulog.F("error", err))
		}
	}()

	if err := sbx.Pause(ctx); err != nil {
		return "", fmt.Errorf("sandbox %s pause failed: %w", req.SandboxId, err)
	}

	// TODO: system sync, too large
	syscall.Sync()

	var key = req.SandboxId

	info, err := snapshot.Stat(ctx, sbx.namespace, key)
	if err != nil {
		return "", fmt.Errorf("failed to stat snapshot %s: %w", key, err)
	}
	parent := info.Parent
	snapshotId, err := snapshot.CalculateSnapshotID(sbx.namespace, key, parent)
	if err != nil {
		return "", fmt.Errorf("failed to calculate snapshot id: %w", err)
	}

	err = snapshot.Commit(context.Background(), sbx.namespace, snapshotId, key)
	if err != nil {
		return "", fmt.Errorf("error committing snapshot %s: %v", req.SandboxId, err)
	}

	return snapshotId, nil
}

func (m *Manager) CleanupPool() error {
	logger := ulog.GetLogger()
	logger.Debug("cleanup pool begin")
	err := m.pool.Cleanup()
	if err != nil {
		return fmt.Errorf("failed to cleanup pool: %v", err)
	}
	logger.Debug("cleanup pool finish")

	return nil
}

func (m *Manager) AllocateUniqueCID(sandboxId string) (uint32, error) {
	return m.cidAllocator.AllocateCID(sandboxId)
}

func (m *Manager) ReleaseCID(sandboxId string) error {
	return m.cidAllocator.ReleaseCID(sandboxId)
}

func (m *Manager) CleanupCIDMap() error {
	return m.cidAllocator.Cleanup()
}
