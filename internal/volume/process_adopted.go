package volume

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type adoptedProcessOps struct {
	readStartTime func(int) uint64
	open          func(int) (int, error)
	poll          func(int, time.Duration) (bool, error)
	kill          func(int) error
	close         func(int) error
}

type adoptedProcess struct {
	pid       int
	startTime uint64
	pidfd     int
	ops       adoptedProcessOps

	closeOnce sync.Once
	closeErr  error
}

func newAdoptedProcess(pid int, startTime uint64) (*adoptedProcess, error) {
	return newAdoptedProcessWithOps(pid, startTime, realAdoptedProcessOps())
}

func newAdoptedProcessWithOps(pid int, startTime uint64, ops adoptedProcessOps) (*adoptedProcess, error) {
	if pid <= 0 || startTime == 0 {
		return nil, fmt.Errorf("invalid adopted virtiofsd identity pid=%d start_time=%d", pid, startTime)
	}
	if ops.readStartTime == nil || ops.open == nil || ops.poll == nil || ops.kill == nil || ops.close == nil {
		return nil, fmt.Errorf("incomplete adopted process operations")
	}
	if current := ops.readStartTime(pid); current != startTime {
		return nil, fmt.Errorf("virtiofsd pid %d identity changed before pidfd_open: recorded=%d current=%d", pid, startTime, current)
	}
	pidfd, err := ops.open(pid)
	if err != nil {
		return nil, fmt.Errorf("pidfd_open virtiofsd pid %d: %w", pid, err)
	}
	if current := ops.readStartTime(pid); current != startTime {
		closeErr := ops.close(pidfd)
		return nil, errors.Join(
			fmt.Errorf("virtiofsd pid %d identity changed after pidfd_open: recorded=%d current=%d", pid, startTime, current),
			closeErr,
		)
	}
	return &adoptedProcess{pid: pid, startTime: startTime, pidfd: pidfd, ops: ops}, nil
}

func realAdoptedProcessOps() adoptedProcessOps {
	return adoptedProcessOps{
		readStartTime: processStartTicks,
		open: func(pid int) (int, error) {
			return unix.PidfdOpen(pid, 0)
		},
		poll: pollPidfd,
		kill: func(pidfd int) error {
			return unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
		},
		close: unix.Close,
	}
}

func (p *adoptedProcess) PID() int { return p.pid }

func (p *adoptedProcess) StartTime() uint64 { return p.startTime }

func (p *adoptedProcess) Wait() processWaitResult {
	ready, err := p.ops.poll(p.pidfd, -1)
	if err != nil {
		return processWaitResult{Exited: false, Cause: err}
	}
	if !ready {
		return processWaitResult{Exited: false, Cause: fmt.Errorf("pidfd poll returned without exit")}
	}
	return processWaitResult{Exited: true}
}

func (p *adoptedProcess) Kill() error {
	err := p.ops.kill(p.pidfd)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func (p *adoptedProcess) ConfirmExit(timeout time.Duration) error {
	ready, err := p.ops.poll(p.pidfd, timeout)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("timed out after %s waiting for pidfd", timeout)
	}
	return nil
}

func (p *adoptedProcess) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = p.ops.close(p.pidfd)
	})
	return p.closeErr
}

func pollPidfd(pidfd int, timeout time.Duration) (bool, error) {
	var deadline time.Time
	if timeout >= 0 {
		deadline = time.Now().Add(timeout)
	}
	fd := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for {
		timeoutMillis := -1
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return false, nil
			}
			millis := remaining.Milliseconds()
			if millis == 0 {
				millis = 1
			}
			if millis > math.MaxInt32 {
				millis = math.MaxInt32
			}
			timeoutMillis = int(millis)
		}
		n, err := unix.Poll(fd, timeoutMillis)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		if n == 0 {
			return false, nil
		}
		if fd[0].Revents&unix.POLLNVAL != 0 {
			return false, unix.EBADF
		}
		return fd[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
	}
}
