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
)

const waitInterval = 10 * time.Millisecond

type Process struct {
	cmd           *exec.Cmd
	VmmSocketPath string
	rootfsPaths   []string
	kernelPath    string
	initrdPath    string
	// Exit *utils.SetOnce[struct{}]
	client vmmClient
	exitSignal chan error
}

func SandboxVmmSocketPath(sandboxId string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("conch-vmm-%s.sock", sandboxId))
}

func NewProcess(
	vmmName, sandboxId string,
	vmmResourceArgs *ResourceArgs, isResume bool,
) (*Process, error) {
	vmmType, exists := GetVmmType(vmmName)
	if !exists {
		return nil, fmt.Errorf("invalid vmm type: %s", vmmName)
	}
	vmmSocketPath := SandboxVmmSocketPath(sandboxId)
	client, err := newVmmClient(vmmType, vmmSocketPath)
	if err != nil {
		return nil, err
	}
	p := Process{
		VmmSocketPath: vmmSocketPath,
		rootfsPaths:   vmmResourceArgs.PmemPaths,
		kernelPath:    vmmResourceArgs.KernelPath,
		initrdPath:    vmmResourceArgs.InitrdPath,
		client:        client,
		exitSignal:    make(chan error, 1),
	}

	startScript, err := client.BuildStartCmd(vmmResourceArgs, isResume)
	if err != nil {
		return nil, fmt.Errorf("failed to Build Start Cmd: %w", err)
	}

	_, err = os.Stat(p.kernelPath)
	if err != nil {
		return nil, fmt.Errorf("error stating kernel file: %w", err)
	}

	_, err = os.Stat(p.initrdPath)
	if err != nil {
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

// waitFile waits for the given file to exist.
func waitFile(ctx context.Context, socketPath string) error {
	ticker := time.NewTicker(waitInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled wait for socket '%s': %w", socketPath, ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err != nil {
				continue
			}

			return nil
		}
	}
}

func (p *Process) startCmd(
	ctx context.Context,
) error {
	// TODO: redirect stderr/stdout
	p.cmd.Stdout = os.Stdout
	p.cmd.Stderr = os.Stderr

	err := p.cmd.Start()
	if err != nil {
		return fmt.Errorf("error starting vmm process: %w", err)
	}

	startCtx, cancelStart := context.WithCancelCause(ctx)
	defer cancelStart(fmt.Errorf("fc finished starting"))

	go func() {
		// TODO: close fd after redirecting stderr/stdout

		waitErr := p.cmd.Wait()
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				// Check if the process was killed by a signal
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() && (status.Signal() == syscall.SIGKILL || status.Signal() == syscall.SIGTERM) {
					fmt.Println("process was killed by a signal")
					p.exitSignal <- nil
					close(p.exitSignal)
					return
				}
			}

			errMsg := fmt.Errorf("error waiting for vmm process: %w", waitErr)
			p.exitSignal <- errMsg
			close(p.exitSignal)
			return
		}
		p.exitSignal <- nil
		close(p.exitSignal)
	}()

	// Wait for the VMM process to start
	err = waitFile(startCtx, p.VmmSocketPath)
	if err != nil {
		errMsg := fmt.Errorf("error waiting for vmm socket: %w", err)
		vmmStopErr := p.Stop()
		return errors.Join(errMsg, vmmStopErr)
	}

	return nil
}

func (p *Process) Create(ctx context.Context) error {
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
	err := p.startCmd(ctx)
	if err != nil {
		vmmStopErr := p.Stop()
		return errors.Join(fmt.Errorf("error starting vmm process: %w", err), vmmStopErr)
	}

	// prefault=false: to achieve fast startup, load memory on demand.
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

	if p.cmd.Process == nil {
		return fmt.Errorf("vmm process not started")
	}

	state, err := getProcessState(p.cmd.Process.Pid)
	if err != nil {
		fmt.Printf("failed to get vmm process state, %s\n", err)
	} else if state == "D" {
		fmt.Println("vmm process is in the D state before we call SIGTERM")
	}

	err = p.cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		return fmt.Errorf("failed to send SIGTERM to vmm process, %s", err)
	}

	<-p.exitSignal
	return nil
}

func (p *Process) Pid() int {
	if p.cmd.Process == nil {
		fmt.Printf("vm process not started")
		return 0
	}

	return p.cmd.Process.Pid
}

func (p *Process) Pause(ctx context.Context) error {
	return p.client.PauseVM()
}

func (p *Process) CreateSnapshot(ctx context.Context, snapfilePath string) error {
	return p.client.CreateSnapshot(snapfilePath)
}

func (p *Process) Wait() error {
	// Blocks until the single reaper goroutine (in startCmd) sends the result.
	// This ensures only one part of the code calls the OS wait syscall.
	err, ok := <-p.exitSignal
	if !ok {
		// Channel closed, process already reaped.
		return nil
	}
	return err
}
