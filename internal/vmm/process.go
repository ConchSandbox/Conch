package vmm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/pkg/ulog"
)

const SocketDirPerm = 0755
const unixSocketPathMax = 107

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
	apiReadyMu      sync.Mutex
	apiReady        bool
	// Exit *utils.SetOnce[struct{}]
	adapter    vmmAdapter
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

	adapter, err := newVmmAdapter(vmmName, vmmSocketPath)
	if err != nil {
		logger.Error("Failed to create VMM adapter", ulog.F("error", err))
		return nil, err
	}

	if err := adapter.PrepareLaunch(vmmResourceArgs, isResume); err != nil {
		logger.Error("Failed to prepare VMM launch", ulog.F("error", err))
		adapter.Cleanup()
		return nil, err
	}

	p := Process{
		VsockSocketPath: vmmResourceArgs.VsockSocketPath,
		VmmSocketPath:   vmmSocketPath,
		adapter:         adapter,
		exitSignal:      make(chan error, 1),
	}

	startScript, err := adapter.BuildStartCmd(vmmResourceArgs, isResume)
	if err != nil {
		logger.Error("Failed to build start command", ulog.F("error", err))
		adapter.Cleanup()
		return nil, fmt.Errorf("failed to Build Start Cmd: %w", err)
	}

	_, err = os.Stat(vmmResourceArgs.KernelPath)
	if err != nil {
		logger.Error("Error stating kernel file", ulog.F("path", vmmResourceArgs.KernelPath), ulog.F("error", err))
		adapter.Cleanup()
		return nil, fmt.Errorf("error stating kernel file: %w", err)
	}

	_, err = os.Stat(vmmResourceArgs.InitrdPath)
	if err != nil {
		logger.Error("Error stating disk file", ulog.F("path", vmmResourceArgs.InitrdPath), ulog.F("error", err))
		adapter.Cleanup()
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
	adapter, err := newVmmAdapter(vmmName, vmmSocketPath)
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
		adapter:         adapter,
		exitSignal:      make(chan error, 1),
	}, nil
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
		p.adapter.Cleanup()
		return fmt.Errorf("error starting vmm process: %w", err)
	}

	p.adapter.AfterProcessStart()

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

func (p *Process) waitForAgentAlive(ctx context.Context) error {
	return p.adapter.CheckAgentAlive(ctx, p.exitSignal)
}

func (p *Process) Create(ctx context.Context) error {
	logger := ulog.GetLogger()

	logger.Debug("Creating VMM")
	err := p.startCmd(ctx)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error starting vmm process: %w", err), vmmStopErr)
	}

	if err := p.adapter.WaitForCreateReady(ctx, p.exitSignal); err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error waiting for vmm create readiness: %w", err), vmmStopErr)
	}
	p.markAPIReady()

	// check conch-init alive
	err = p.waitForAgentAlive(ctx)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error starting conch-init in vmm: %w", err), vmmStopErr)
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

	if err := p.adapter.WaitForResumeReady(ctx, p.exitSignal); err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error waiting for vmm resume readiness: %w", err), vmmStopErr)
	}

	// preferVNC=false: to achieve fast startup, load memory on demand.
	err = p.adapter.LoadSnapshot(snapfilePath, false)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error loading snapshot: %w", err), vmmStopErr)
	}
	p.markAPIReady()

	err = p.adapter.ResumeVM()
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error resuming vm: %w", err), vmmStopErr)
	}

	// check conch-init alive
	err = p.waitForAgentAlive(ctx)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error starting conch-init in vmm: %w", err), vmmStopErr)
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
					if deleteErr := p.adapter.DeleteVM(); deleteErr != nil {
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
		p.adapter.Cleanup()
		logger.Warn("VMM process not started")
		return fmt.Errorf("vmm process not started")
	}

	select {
	case <-p.exitSignal:
		// Already exited
		p.adapter.Cleanup()
		return errors.Join(errs...)
	default:
	}

	if p.isAPIReady() {
		if _, err := os.Stat(p.VmmSocketPath); err == nil {
			if deleteErr := p.adapter.DeleteVM(); deleteErr != nil {
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
	p.adapter.Cleanup()
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
	return p.adapter.PauseVM()
}

func (p *Process) ResumeVM(ctx context.Context) error {
	if err := p.adapter.ResumeVM(); err != nil {
		return err
	}
	return p.waitForAgentAlive(ctx)
}

func (p *Process) CreateSnapshot(ctx context.Context, snapfilePath string) error {
	logger := ulog.GetLogger()
	logger.Info("Creating snapshot",
		ulog.F("path", snapfilePath),
	)
	return p.adapter.CreateSnapshot(snapfilePath)
}

func (p *Process) Wait() error {
	logger := ulog.GetLogger()

	// Blocks until single reaper goroutine (in startCmd) sends result.
	// This ensures only one part of code calls OS wait syscall.
	err, ok := <-p.exitSignal
	if !ok {
		// Channel closed, process already reaped.
		logger.Debug("Process already reaped")
		p.adapter.Cleanup()
		return nil
	}
	p.adapter.Cleanup()
	if err != nil {
		logger.Error("VMM process wait error",
			ulog.F("error", err),
		)
		return err
	}
	return nil
}
