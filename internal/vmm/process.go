package vmm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/pkg/ulog"
)

const SocketDirPerm = 0755
const unixSocketPathMax = 107

// Cloud-hypervisor event types
const (
	EventBooted = "booted"
)

// EnsureWorkSubDir creates a subdirectory under WorkDir and returns its path.
func EnsureWorkSubDir(subDir string) (string, error) {
	workDir := config.WorkDir
	if !filepath.IsAbs(workDir) {
		return "", fmt.Errorf("WorkDir must be an absolute path, got: %s", workDir)
	}
	dir := filepath.Join(workDir, subDir)
	if err := os.MkdirAll(dir, SocketDirPerm); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	return dir, nil
}

// SandboxSocketPath returns a short Unix socket path for sandbox-scoped VMM resources.
func SandboxSocketPath(subDir, sandboxId string) (string, error) {
	socketDir, err := EnsureWorkSubDir(subDir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(subDir + ":" + sandboxId))
	name := hex.EncodeToString(sum[:8]) + ".sock"
	path := filepath.Join(socketDir, name)
	if len(path) > unixSocketPathMax {
		return "", fmt.Errorf("sandbox socket path length %d exceeds unix socket limit %d: %s; configure a shorter server.work_dir", len(path), unixSocketPathMax, path)
	}
	return path, nil
}

type Process struct {
	cmd             *exec.Cmd
	pid             int
	attached        bool
	VmmSocketPath   string
	VsockSocketPath string
	vmmFds          *VmmFds
	apiReadyMu      sync.Mutex
	apiReady        bool
	rootfsPaths     []string
	kernelPath      string
	initrdPath      string
	// Exit *utils.SetOnce[struct{}]
	client     vmmClient
	exitSignal chan error
}

func SandboxVmmSocketPath(sandboxId string) (string, error) {
	return SandboxSocketPath("v", sandboxId)
}

func (p *Process) markAPIReady() {
	p.apiReadyMu.Lock()
	defer p.apiReadyMu.Unlock()
	p.apiReady = true
}

func (p *Process) isAPIReady() bool {
	p.apiReadyMu.Lock()
	defer p.apiReadyMu.Unlock()
	return p.apiReady
}

func (p *Process) cleanupVmmFds() {
	if p.vmmFds != nil {
		p.vmmFds.cleanup()
	}
}

// VmmFds holds file descriptors for communicating with cloud-hypervisor
type VmmFds struct {
	mu           sync.Mutex
	conchEventFd int
	clhEventFd   int
	apiSocketFd  int
	socketPath   string // for cleanup
}

func closeFd(fd *int) {
	if *fd > 0 {
		_ = unix.Close(*fd)
		*fd = 0
	}
}

// closeChildFdsInParent closes the parent's copies of the descriptors inherited
// by cloud-hypervisor. The child process keeps its copies after Start.
func (f *VmmFds) closeChildFdsInParent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	closeFd(&f.clhEventFd)
	closeFd(&f.apiSocketFd)
}

// cleanup closes all fds and removes the socket file.
func (f *VmmFds) cleanup() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	closeFd(&f.conchEventFd)
	closeFd(&f.clhEventFd)
	closeFd(&f.apiSocketFd)
	if f.socketPath != "" {
		_ = unix.Unlink(f.socketPath)
		f.socketPath = ""
	}
}

// createVmmFds creates file descriptors needed for cloud-hypervisor communication:
// - event-monitor socketpair
// - api-socket unix socket (bind + listen)
func createVmmFds(vmmSocketPath string) (*VmmFds, error) {
	vmmFds := &VmmFds{socketPath: vmmSocketPath}

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		vmmFds.cleanup()
		return nil, fmt.Errorf("failed to create socketpair: %w", err)
	}
	vmmFds.conchEventFd = fds[0]
	vmmFds.clhEventFd = fds[1]

	// Set conchd fd to close-on-exec (cloud-hypervisor fd should NOT be close-on-exec)
	unix.CloseOnExec(vmmFds.conchEventFd)

	vmmFds.apiSocketFd, err = unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		vmmFds.cleanup()
		return nil, fmt.Errorf("failed to create api socket: %w", err)
	}

	sockAddr := &unix.SockaddrUnix{Name: vmmSocketPath}
	if err := unix.Bind(vmmFds.apiSocketFd, sockAddr); err != nil {
		vmmFds.cleanup()
		return nil, fmt.Errorf("failed to bind api socket: %w", err)
	}
	if err := unix.Listen(vmmFds.apiSocketFd, 1); err != nil {
		vmmFds.cleanup()
		return nil, fmt.Errorf("failed to listen on api socket: %w", err)
	}

	return vmmFds, nil
}

func NewProcess(
	vmmName, sandboxId string,
	vmmResourceArgs *ResourceArgs, isResume bool,
) (*Process, error) {
	logger := ulog.GetLogger()

	vmmSocketPath, err := SandboxVmmSocketPath(sandboxId)
	if err != nil {
		logger.Error("Failed to get VMM socket path", ulog.F("error", err))
		return nil, err
	}

	vmmType, exists := GetVmmType(vmmName)
	if !exists {
		logger.Error("Invalid VMM type", ulog.F("vmm_name", vmmName))
		return nil, fmt.Errorf("invalid vmm type: %s", vmmName)
	}

	var vmmFds *VmmFds
	if vmmType == CLHVmmType {
		vmmFds, err = createVmmFds(vmmSocketPath)
		if err != nil {
			logger.Error("Failed to create vmm fds", ulog.F("error", err))
			return nil, err
		}

		vmmResourceArgs.EventMonitorFd = vmmFds.clhEventFd
		vmmResourceArgs.ApiSocketFd = vmmFds.apiSocketFd
	}

	client, err := newVmmClient(vmmType, vmmSocketPath)
	if err != nil {
		logger.Error("Failed to create VMM client", ulog.F("error", err))
		vmmFds.cleanup()
		return nil, err
	}

	p := Process{
		VsockSocketPath: vmmResourceArgs.VsockSocketPath,
		vmmFds:          vmmFds,
		VmmSocketPath:   vmmSocketPath,
		rootfsPaths:     vmmResourceArgs.PmemPaths,
		kernelPath:      vmmResourceArgs.KernelPath,
		initrdPath:      vmmResourceArgs.InitrdPath,
		client:          client,
		exitSignal:      make(chan error, 1),
	}

	startScript, err := client.BuildStartCmd(vmmResourceArgs, isResume)
	if err != nil {
		logger.Error("Failed to build start command", ulog.F("error", err))
		vmmFds.cleanup()
		return nil, fmt.Errorf("failed to Build Start Cmd: %w", err)
	}

	_, err = os.Stat(p.kernelPath)
	if err != nil {
		logger.Error("Error stating kernel file", ulog.F("path", p.kernelPath), ulog.F("error", err))
		vmmFds.cleanup()
		return nil, fmt.Errorf("error stating kernel file: %w", err)
	}

	_, err = os.Stat(p.initrdPath)
	if err != nil {
		logger.Error("Error stating disk file", ulog.F("path", p.initrdPath), ulog.F("error", err))
		vmmFds.cleanup()
		return nil, fmt.Errorf("error stating disk file: %w", err)
	}

	cmd := exec.Command(
		"bash",
		"-c",
		startScript,
	)
	p.cmd = cmd

	return &p, nil
}

func NewAttachedProcess(vmmName, vmmSocketPath, vsockSocketPath string, pid int) (*Process, error) {
	vmmType, exists := GetVmmType(vmmName)
	if !exists {
		return nil, fmt.Errorf("invalid vmm type: %s", vmmName)
	}
	client, err := newVmmClient(vmmType, vmmSocketPath)
	if err != nil {
		return nil, err
	}
	// Rehydrated processes are already ready; startup-only event-monitor fds cannot be restored.
	return &Process{
		pid:             pid,
		attached:        true,
		VmmSocketPath:   vmmSocketPath,
		VsockSocketPath: vsockSocketPath,
		apiReady:        true,
		client:          client,
		exitSignal:      make(chan error, 1),
	}, nil
}

// CloudHypervisorEvent represents an event from cloud-hypervisor event-monitor
type CloudHypervisorEvent struct {
	Timestamp interface{} `json:"timestamp"`
	Source    string      `json:"source"`
	Event     string      `json:"event"`
}

// parseEventsFromFd reads and parses JSON events from a file descriptor.
// It returns all parsed events or an error if parsing fails.
func parseEventsFromFd(eventFd int, buf []byte) ([]CloudHypervisorEvent, error) {
	readN, readErr := unix.Read(eventFd, buf)
	if readN <= 0 {
		if readErr == unix.EAGAIN || readErr == unix.EWOULDBLOCK {
			return nil, nil
		}
		if readErr != nil {
			return nil, fmt.Errorf("read error: %w", readErr)
		}
		return nil, io.EOF
	}

	var events []CloudHypervisorEvent
	decoder := json.NewDecoder(strings.NewReader(string(buf[:readN])))
	for {
		var event CloudHypervisorEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				return events, nil
			}
			return events, fmt.Errorf("decode event monitor payload: %w", err)
		}
		events = append(events, event)
	}
}

func (p *Process) startCmd(
	ctx context.Context,
) error {
	logger := ulog.GetLogger()

	// TODO: redirect stderr/stdout
	p.cmd.Stdout = os.Stdout
	p.cmd.Stderr = os.Stderr

	logger.Debug("Starting VMM process")
	err := p.cmd.Start()
	if err != nil {
		logger.Error("Error starting VMM process",
			ulog.F("error", err),
		)
		p.cleanupVmmFds()
		return fmt.Errorf("error starting vmm process: %w", err)
	}

	if p.vmmFds != nil {
		p.vmmFds.closeChildFdsInParent()
	}

	go func() {
		// TODO: close fd after redirecting stderr/stdout
		waitErr := p.cmd.Wait()
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				// Check if process was killed by a signal
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() && (status.Signal() == syscall.SIGKILL || status.Signal() == syscall.SIGTERM) {
					logger.Debug("VMM process killed by signal")
					p.exitSignal <- nil
					close(p.exitSignal)
					return
				}
			}
			errMsg := fmt.Errorf("error waiting for vmm process: %w", waitErr)
			logger.Warn("VMM process error",
				ulog.F("error", errMsg),
			)
			p.exitSignal <- errMsg
			close(p.exitSignal)
			return
		}
		logger.Debug("VMM process exited normally")
		p.exitSignal <- nil
		close(p.exitSignal)
	}()

	return nil
}

// waitForSourceEvent waits for a specific event from a specific source.
// It stops the VMM process if waiting fails.
func (p *Process) waitForSourceEvent(ctx context.Context, source, eventName string) error {
	if p.vmmFds == nil || p.vmmFds.conchEventFd <= 0 {
		return nil
	}

	logger := ulog.GetLogger()
	logger.Info("Waiting for VM event", ulog.F("event_fd", p.vmmFds.conchEventFd), ulog.F("source", source), ulog.F("event", eventName))
	err := waitVmReadyFd(ctx, p.vmmFds.conchEventFd, source, eventName)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error waiting for %s/%s event: %w", source, eventName, err), vmmStopErr)
	}

	return nil
}

// waitVmReadyFd waits for the VM to be ready by reading events from fd.
// It watches for the specified event which indicates the VM has started successfully.
func waitVmReadyFd(ctx context.Context, eventFd int, waitForSource, waitForEvent string) error {
	epollFd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("failed to create epoll: %w", err)
	}
	defer unix.Close(epollFd)

	epollEvent := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(eventFd)}
	if err := unix.EpollCtl(epollFd, unix.EPOLL_CTL_ADD, eventFd, &epollEvent); err != nil {
		return fmt.Errorf("failed to add event fd to epoll: %w", err)
	}

	events := make([]unix.EpollEvent, 1)
	buf := make([]byte, 4096)

	for {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled waiting for VM ready: %w", ctx.Err())
		}

		timeoutMs := -1 // -1 means wait indefinitely
		if deadline, ok := ctx.Deadline(); ok {
			timeoutMs = int(time.Until(deadline).Milliseconds())
			if timeoutMs <= 0 {
				return fmt.Errorf("timeout waiting for VM ready event")
			}
		}

		n, err := unix.EpollWait(epollFd, events, timeoutMs)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("epoll wait error: %w", err)
		}

		if n == 0 {
			return fmt.Errorf("timeout waiting for VM ready event")
		}

		clhEvents, err := parseEventsFromFd(eventFd, buf)
		if err != nil {
			return err
		}
		for _, event := range clhEvents {
			if event.Source == waitForSource && event.Event == waitForEvent {
				return nil
			}
		}
	}
}

// waitForVmmSocket waits for VMM-managed Unix sockets, such as StratoVirt's QMP
// socket. Cloud-hypervisor uses a pre-bound fd API socket and does not need this.
func (p *Process) waitForVmmSocket(ctx context.Context) error {
	logger := ulog.GetLogger()

	delay := 2 * time.Millisecond
	const maxDelay = 100 * time.Millisecond
	for {
		if _, err := os.Stat(p.VmmSocketPath); err == nil {
			logger.Debug("VMM socket ready", ulog.F("socket", p.VmmSocketPath))
			return nil
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("cancelled waiting for vmm socket %s: %w", p.VmmSocketPath, ctx.Err())
		case <-timer.C:
		}

		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (p *Process) Create(ctx context.Context) error {
	logger := ulog.GetLogger()

	logger.Debug("Creating VMM")
	err := p.startCmd(ctx)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error starting vmm process: %w", err), vmmStopErr)
	}

	// Wait for VM boot event
	logger.Info("Waiting for VM boot event")
	if err := p.waitForSourceEvent(ctx, "vm", EventBooted); err != nil {
		return err
	}
	logger.Info("VM booted, ready for vsock")

	if p.vmmFds == nil {
		if err := p.waitForVmmSocket(ctx); err != nil {
			vmmStopErr := p.Stop()
			return errors.Join(fmt.Errorf("error waiting for vmm socket: %w", err), vmmStopErr)
		}
	}
	p.markAPIReady()

	// check conchd alive
	err = p.client.CheckDaemonAlive()
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error starting daemon in vmm: %w", err), vmmStopErr)
	}

	return nil
}

func (p *Process) Resume(ctx context.Context, snapfilePath string) error {
	logger := ulog.GetLogger()

	logger.Info("Resuming VMM from snapshot",
		ulog.F("snapshot", snapfilePath),
	)

	err := p.startCmd(ctx)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error starting vmm process: %w", err), vmmStopErr)
	}

	// With fd-based api socket, no need to wait for api-ready event. StratoVirt
	// creates its QMP socket after process start, so wait before QMP calls.
	if p.vmmFds == nil {
		if err := p.waitForVmmSocket(ctx); err != nil {
			vmmStopErr := p.Stop()
			return errors.Join(fmt.Errorf("error waiting for vmm socket: %w", err), vmmStopErr)
		}
	}

	// preferVNC=false: to achieve fast startup, load memory on demand.
	err = p.client.LoadSnapshot(snapfilePath, false)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error loading snapshot: %w", err), vmmStopErr)
	}
	p.markAPIReady()

	err = p.client.ResumeVM()
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error resuming vm: %w", err), vmmStopErr)
	}

	// check conchd alive
	err = p.client.CheckDaemonAlive()
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error starting daemon in vmm: %w", err), vmmStopErr)
	}

	return nil
}

func getProcessState(pid int) (string, error) {
	cmd, err := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return "", err
	}

	state := strings.TrimSpace(string(cmd))
	return state, nil
}

func (p *Process) Stop() error {
	logger := ulog.GetLogger()
	var errs []error

	if p.cmd == nil || p.cmd.Process == nil {
		if p.attached {
			if p.isAPIReady() {
				if _, err := os.Stat(p.VmmSocketPath); err == nil {
					if deleteErr := p.client.DeleteVM(); deleteErr != nil {
						errs = append(errs, fmt.Errorf("delete vmm via api: %w", deleteErr))
					}
				}
			}
			if p.pid > 0 {
				if err := syscall.Kill(p.pid, syscall.SIGTERM); err != nil {
					if !errors.Is(err, syscall.ESRCH) {
						errs = append(errs, fmt.Errorf("failed to send SIGTERM to attached vmm process, %d: %w", p.pid, err))
					}
				}
			}
			return errors.Join(errs...)
		}
		p.cleanupVmmFds()
		logger.Warn("VMM process not started")
		return fmt.Errorf("vmm process not started")
	}

	select {
	case <-p.exitSignal:
		// Already exited
		p.cleanupVmmFds()
		return errors.Join(errs...)
	default:
	}

	if p.isAPIReady() {
		if _, err := os.Stat(p.VmmSocketPath); err == nil {
			if deleteErr := p.client.DeleteVM(); deleteErr != nil {
				errs = append(errs, fmt.Errorf("delete vmm via api: %w", deleteErr))
			}
		}
	}

	state, err := getProcessState(p.cmd.Process.Pid)
	if err != nil {
		logger.Error("Failed to get VMM process state",
			ulog.F("pid", p.cmd.Process.Pid),
			ulog.F("error", err),
		)
	} else if state == "D" {
		logger.Debug("VMM process in D state before SIGTERM")
	}

	err = p.cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		logger.Error("Failed to send SIGTERM to VMM process",
			ulog.F("pid", p.cmd.Process.Pid),
			ulog.F("error", err),
		)
		errs = append(errs, fmt.Errorf("failed to send SIGTERM to vmm process, %d: %w", p.cmd.Process.Pid, err))
		return errors.Join(errs...)
	}

	logger.Debug("Sent SIGTERM to VMM process",
		ulog.F("pid", p.cmd.Process.Pid),
	)

	<-p.exitSignal
	p.cleanupVmmFds()
	return errors.Join(errs...)
}

func (p *Process) Pid() int {
	if p.pid > 0 {
		return p.pid
	}
	if p.cmd == nil || p.cmd.Process == nil {
		logger := ulog.GetLogger()
		logger.Warn("VMM process not started")
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *Process) Pause(ctx context.Context) error {
	return p.client.PauseVM()
}

func (p *Process) CreateSnapshot(ctx context.Context, snapfilePath string) error {
	logger := ulog.GetLogger()
	logger.Info("Creating snapshot",
		ulog.F("path", snapfilePath),
	)
	return p.client.CreateSnapshot(snapfilePath)
}

func (p *Process) Wait() error {
	logger := ulog.GetLogger()

	// Blocks until single reaper goroutine (in startCmd) sends result.
	// This ensures only one part of code calls OS wait syscall.
	err, ok := <-p.exitSignal
	if !ok {
		// Channel closed, process already reaped.
		logger.Debug("Process already reaped")
		p.cleanupVmmFds()
		return nil
	}
	p.cleanupVmmFds()
	if err != nil {
		logger.Error("VMM process wait error",
			ulog.F("error", err),
		)
		return err
	}
	return nil
}
