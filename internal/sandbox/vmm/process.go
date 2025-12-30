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
	rootfsPath    string
	kernelPath    string
	diskPath    string
	// Exit *utils.SetOnce[struct{}]
	client vmmClient
}

func SandboxVmmSocketPath(sandboxId string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("conch-vmm-%s.sock", sandboxId))
}

func NewProcess(
	rootfsPaths, memFile, rootfsSock, kernelPath, diskPath,
	vmmName, sandboxId, namespaceID, tapName string,
	memSize int64, isResume bool,
) (*Process, error) {
	// debug
	fmt.Println("New process...")

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
		rootfsPath: rootfsPaths,
		kernelPath: kernelPath,
		diskPath: diskPath,
		client: client,
	}

	vmmResourceArgs := &ResourceArgs {
		CPUBoot: 1,
		CPUMax: 1,
		MemorySize: memSize,
		MemoryPath: memFile,
		NamespaceID: namespaceID,
		TapName: tapName,
	}

	startScript, err := client.BuildStartCmd(vmmResourceArgs, rootfsSock, kernelPath, diskPath, isResume)
	if err != nil {
		return nil, fmt.Errorf("failed to Build Start Cmd: %w", err)
	}

	_, err = os.Stat(p.kernelPath)
	if err != nil {
		return nil, fmt.Errorf("error stating kernel file: %w", err)
	}

	_, err = os.Stat(p.diskPath)
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

	// debug
	fmt.Printf("begin Start vm *** %d ***\n", time.Now().UnixNano()/int64(time.Millisecond))
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
					return
				}
			}

			errMsg := fmt.Errorf("error waiting for vmm process: %w", waitErr)
			fmt.Println(errMsg)
			cancelStart(errMsg)

			return
		}
	}()

	// debug
	fmt.Println("Waiting vmm socket...")
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
	// debug
	fmt.Println("Creating vmm...")

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

	// TODO: goroutine Wait 10 sec for the VMM process to exit, if it doesn't, send SIGKILL.
	_, err = p.cmd.Process.Wait()
	if err != nil {
		// debug log
		fmt.Printf("Wait process: %s", err)
	}

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
	_, err := p.cmd.Process.Wait()
	if err != nil {
		// debug log
		fmt.Printf("Wait process: %s", err)
	}
	return nil
}