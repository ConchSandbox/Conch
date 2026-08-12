package volume

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type childProcess struct {
	cmd       *exec.Cmd
	pid       int
	startTime uint64

	waitDone chan struct{}
	waitOnce sync.Once
	result   processWaitResult
}

func newChildProcess(cmd *exec.Cmd, startTime uint64) (*childProcess, error) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil, fmt.Errorf("virtiofsd child process has not started")
	}
	return &childProcess{
		cmd:       cmd,
		pid:       cmd.Process.Pid,
		startTime: startTime,
		waitDone:  make(chan struct{}),
	}, nil
}

func (p *childProcess) PID() int { return p.pid }

func (p *childProcess) StartTime() uint64 { return p.startTime }

func (p *childProcess) Wait() processWaitResult {
	p.waitOnce.Do(func() {
		p.result = childWaitResult(p.cmd.Wait())
		close(p.waitDone)
	})
	return p.result
}

func childWaitResult(waitErr error) processWaitResult {
	result := processWaitResult{Exited: true, Cause: waitErr}
	if waitErr == nil {
		exitCode := 0
		result.ExitCode = &exitCode
		return result
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		// cmd.Wait errors other than ExitError do not prove that the child was
		// reaped (for example, an invalid/double Wait invocation).
		result.Exited = false
		return result
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return result
	}
	if status.Signaled() {
		result.Signal = unix.SignalName(status.Signal())
		return result
	}
	exitCode := status.ExitStatus()
	result.ExitCode = &exitCode
	return result
}

func (p *childProcess) Kill() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func (p *childProcess) ConfirmExit(timeout time.Duration) error {
	if waitForDone(p.waitDone, timeout) {
		return nil
	}
	return fmt.Errorf("timed out after %s waiting for child process", timeout)
}

func (p *childProcess) Close() error { return nil }
