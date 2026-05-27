package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/pkg/ulog"
)

const SocketDirPerm = 0755

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

type Process struct {
	cmd             *exec.Cmd
	VmmSocketPath   string
	VsockSocketPath string
	vmmFds          *VmmFds
	rootfsPaths     []string
	kernelPath      string
	initrdPath      string
	// Exit *utils.SetOnce[struct{}]
	client     vmmClient
	exitSignal chan error
}

func SandboxVmmSocketPath(sandboxId string) (string, error) {
	socketDir, err := EnsureWorkSubDir("vmm")
	if err != nil {
		return "", err
	}
	return filepath.Join(socketDir, fmt.Sprintf("conch-vmm-%s.sock", sandboxId)), nil
}

// VmmFds holds file descriptors for communicating with cloud-hypervisor
type VmmFds struct {
	conchEventFd int
	clhEventFd   int
	apiSocketFd  int
	socketPath   string // for cleanup
}

// cleanup closes all fds and removes the socket file.
// Each fd is checked before closing to avoid double-close.
func (f *VmmFds) cleanup() {
	if f.conchEventFd > 0 {
		unix.Close(f.conchEventFd)
		f.conchEventFd = 0
	}
	if f.clhEventFd > 0 {
		unix.Close(f.clhEventFd)
		f.clhEventFd = 0
	}
	if f.apiSocketFd > 0 {
		unix.Close(f.apiSocketFd)
		f.apiSocketFd = 0
	}
	if f.socketPath != "" {
		unix.Unlink(f.socketPath)
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

	vmmFds, err := createVmmFds(vmmSocketPath)
	if err != nil {
		logger.Error("Failed to create vmm fds", ulog.F("error", err))
		return nil, err
	}

	vmmResourceArgs.EventMonitorFd = vmmFds.clhEventFd
	vmmResourceArgs.ApiSocketFd = vmmFds.apiSocketFd

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
		"unshare",
		"-m",
		"--",
		"bash",
		"-c",
		startScript,
	)
	p.cmd = cmd

	return &p, nil
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
		return nil, nil
	}

	var events []CloudHypervisorEvent
	decoder := json.NewDecoder(strings.NewReader(string(buf[:readN])))
	for decoder.More() {
		var event CloudHypervisorEvent
		if err := decoder.Decode(&event); err != nil {
			break
		}
		events = append(events, event)
	}

	return events, nil
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
		return fmt.Errorf("error starting vmm process: %w", err)
	}

	// Note: we don't close conchEventFd here, only the clh side
	// The clh fd is passed via command line and inherited by the child process

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
			p.exitSignal <- nil
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

	// With fd-based api socket, no need to wait for api-ready event
	// The socket is already bound and listening when cloud-hypervisor starts

	// preferVNC=false: to achieve fast startup, load memory on demand.
	err = p.client.LoadSnapshot(snapfilePath, false)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error loading snapshot: %w", err), vmmStopErr)
	}

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
	select {
	case <-p.exitSignal:
		// Already exited
		return nil
	default:
	}

	logger := ulog.GetLogger()

	if p.cmd.Process == nil {
		logger.Warn("VMM process not started")
		return fmt.Errorf("vmm process not started")
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
		return fmt.Errorf("failed to send SIGTERM to vmm process, %s: %w", p.cmd.Process.Pid, err)
	}

	logger.Debug("Sent SIGTERM to VMM process",
		ulog.F("pid", p.cmd.Process.Pid),
	)

	// Cleanup file descriptors and socket
	if p.vmmFds != nil {
		p.vmmFds.cleanup()
	}

	<-p.exitSignal
	return nil
}

func (p *Process) Pid() int {
	if p.cmd.Process == nil {
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
		return nil
	}
	if err != nil {
		logger.Error("VMM process wait error",
			ulog.F("error", err),
		)
	}
	return err
}
