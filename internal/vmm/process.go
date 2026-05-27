package vmm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/pkg/ulog"
)

const waitInterval = 10 * time.Millisecond

const SocketDirPerm = 0755

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
	pid             int
	attached        bool
	VmmSocketPath   string
	VsockSocketPath string
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

	client, err := newVmmClient(vmmType, vmmSocketPath)
	if err != nil {
		logger.Error("Failed to create VMM client",
			ulog.F("error", err),
		)
		return nil, err
	}

	p := Process{
		VmmSocketPath:   vmmSocketPath,
		VsockSocketPath: vmmResourceArgs.VsockSocketPath,
		rootfsPaths:     vmmResourceArgs.PmemPaths,
		kernelPath:      vmmResourceArgs.KernelPath,
		initrdPath:      vmmResourceArgs.InitrdPath,
		client:          client,
		exitSignal:      make(chan error, 1),
	}

	startScript, err := client.BuildStartCmd(vmmResourceArgs, isResume)
	if err != nil {
		logger.Error("Failed to build start command",
			ulog.F("error", err),
		)
		return nil, fmt.Errorf("failed to Build Start Cmd: %w", err)
	}

	_, err = os.Stat(p.kernelPath)
	if err != nil {
		logger.Error("Error stating kernel file",
			ulog.F("path", p.kernelPath),
			ulog.F("error", err),
		)
		return nil, fmt.Errorf("error stating kernel file: %w", err)
	}

	_, err = os.Stat(p.initrdPath)
	if err != nil {
		logger.Error("Error stating disk file",
			ulog.F("path", p.initrdPath),
			ulog.F("error", err),
		)
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
	// case Operation not permitted
	// cmd.SysProcAttr = &syscall.SysProcAttr{
	// 	Setsid: true, // Create a new session
	// }
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
	return &Process{
		pid:             pid,
		attached:        true,
		VmmSocketPath:   vmmSocketPath,
		VsockSocketPath: vsockSocketPath,
		client:          client,
		exitSignal:      make(chan error, 1),
	}, nil
}

// waitFile waits for the given file to exist.
func waitFile(ctx context.Context, socketPath string) error {
	logger := ulog.GetLogger()
	logger.Debug("Waiting for VMM socket", ulog.F("socket", socketPath))

	ticker := time.NewTicker(waitInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Warn("Cancelled while waiting for VMM socket",
				ulog.F("socket", socketPath),
				ulog.F("error", ctx.Err()),
			)
			return fmt.Errorf("cancelled wait for socket '%s': %w", socketPath, ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err != nil {
				continue
			}
			logger.Debug("VMM socket ready", ulog.F("socket", socketPath))
			return nil
		}
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
		return fmt.Errorf("error starting vmm process: %w", err)
	}

	startCtx, cancelStart := context.WithCancelCause(ctx)
	defer cancelStart(fmt.Errorf("vmm finished starting"))

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
			cancelStart(errMsg)
			p.exitSignal <- nil
			close(p.exitSignal)
			return
		}
		logger.Debug("VMM process exited normally")
		p.exitSignal <- nil
		close(p.exitSignal)
	}()

	logger.Info("Waiting for VMM socket")
	// Wait for the VMM process to start
	err = waitFile(startCtx, p.VmmSocketPath)
	if err != nil {
		errMsg := fmt.Errorf("error waiting for vmm socket: %w", err)
		logger.Error("Error waiting for VMM socket",
			ulog.F("error", err),
		)
		vmmStopErr := p.Stop()
		return errors.Join(errMsg, vmmStopErr)
	}

	return nil
}

func (p *Process) Create(ctx context.Context) error {
	logger := ulog.GetLogger()

	logger.Debug("Creating VMM")
	err := p.startCmd(ctx)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error starting vmm process: %w", err), vmmStopErr)
	}

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
	var errs []error
	if _, err := os.Stat(p.VmmSocketPath); err == nil {
		if deleteErr := p.client.DeleteVM(); deleteErr != nil {
			errs = append(errs, fmt.Errorf("delete vmm via api: %w", deleteErr))
		}
	}

	select {
	case <-p.exitSignal:
		// Already exited
		return errors.Join(errs...)
	default:
	}

	logger := ulog.GetLogger()

	if p.cmd == nil || p.cmd.Process == nil {
		if p.attached {
			if p.pid > 0 {
				if err := syscall.Kill(p.pid, syscall.SIGTERM); err != nil {
					if !errors.Is(err, syscall.ESRCH) {
						errs = append(errs, fmt.Errorf("failed to send SIGTERM to attached vmm process, %d: %w", p.pid, err))
					}
				}
			}
			return errors.Join(errs...)
		}
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
		errs = append(errs, fmt.Errorf("failed to send SIGTERM to vmm process, %d: %w", p.cmd.Process.Pid, err))
		return errors.Join(errs...)
	}

	logger.Debug("Sent SIGTERM to VMM process",
		ulog.F("pid", p.cmd.Process.Pid),
	)

	<-p.exitSignal
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
		return nil
	}
	if err != nil {
		logger.Error("VMM process wait error",
			ulog.F("error", err),
		)
		return err
	}

	ticker := time.NewTicker(waitInterval)
	defer ticker.Stop()
	for {
		if _, statErr := os.Stat(p.VmmSocketPath); statErr != nil {
			if os.IsNotExist(statErr) {
				return nil
			}
			return fmt.Errorf("stat vmm socket: %w", statErr)
		}
		<-ticker.C
	}
}
