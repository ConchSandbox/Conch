package cow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	processStartupTimeout = 5 * time.Second
	processReadyRetry     = 10 * time.Millisecond
)

// Process owns one conch-cow child started by conchd.
type Process struct {
	cmd      *exec.Cmd
	waitDone chan error

	closeOnce sync.Once
	closeErr  error
}

// StartProcess starts conch-cow and waits until its control protocol responds.
func StartProcess(ctx context.Context, binaryPath, socketPath string) (*Process, error) {
	if binaryPath == "" {
		return nil, fmt.Errorf("cow binary path is required")
	}
	if socketPath == "" {
		return nil, fmt.Errorf("cow socket path is required")
	}

	cmd := exec.Command(binaryPath, "--socket", socketPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start conch-cow: %w", err)
	}
	process := &Process{cmd: cmd, waitDone: make(chan error, 1)}
	go func() {
		process.waitDone <- cmd.Wait()
	}()

	startupCtx, cancel := context.WithTimeout(ctx, processStartupTimeout)
	defer cancel()
	client := NewClient(socketPath)
	var lastErr error
	for {
		if _, err := client.Capabilities(startupCtx); err == nil {
			return process, nil
		} else {
			lastErr = err
		}

		timer := time.NewTimer(processReadyRetry)
		select {
		case waitErr := <-process.waitDone:
			timer.Stop()
			if waitErr == nil {
				return nil, fmt.Errorf("conch-cow exited before becoming ready")
			}
			return nil, fmt.Errorf("conch-cow exited before becoming ready: %w", waitErr)
		case <-startupCtx.Done():
			timer.Stop()
			cleanupErr := process.stopAndWait()
			return nil, errors.Join(fmt.Errorf("wait for conch-cow readiness: %w: %v", startupCtx.Err(), lastErr), cleanupErr)
		case <-timer.C:
		}
	}
}

// Close asks conch-cow to stop normally and reaps the child process.
func (process *Process) Close() error {
	if process == nil {
		return nil
	}
	process.closeOnce.Do(func() {
		process.closeErr = process.stopAndWait()
	})
	return process.closeErr
}

func (process *Process) stopAndWait() error {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return nil
	}
	select {
	case waitErr := <-process.waitDone:
		if waitErr != nil {
			return fmt.Errorf("wait for conch-cow: %w", waitErr)
		}
		return nil
	default:
	}

	signalErr := process.cmd.Process.Signal(syscall.SIGTERM)
	if errors.Is(signalErr, os.ErrProcessDone) {
		signalErr = nil
	}
	waitErr := <-process.waitDone
	if waitErr != nil {
		waitErr = fmt.Errorf("wait for conch-cow: %w", waitErr)
	}
	return errors.Join(signalErr, waitErr)
}
