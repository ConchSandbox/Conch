package volume

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const processCloseTimeout = 3 * time.Second

type processWaitResult struct {
	Exited   bool
	Cause    error
	ExitCode *int
	Signal   string
}

type processHandle interface {
	PID() int
	StartTime() uint64
	Wait() processWaitResult
	Kill() error
	ConfirmExit(timeout time.Duration) error
	Close() error
}

// virtiofsProcess owns the one Wait call for a child or adopted process. The
// monitor publishes an immutable observation before closing observeDone.
type virtiofsProcess struct {
	sandboxID string
	handle    processHandle

	observeDone chan struct{}

	mu          sync.Mutex
	observation ProcessObservation
	observed    bool

	stopOnce sync.Once
	stopErr  error
}

func newVirtiofsProcess(sandboxID string, handle processHandle) *virtiofsProcess {
	return &virtiofsProcess{
		sandboxID:   sandboxID,
		handle:      handle,
		observeDone: make(chan struct{}),
	}
}

func (p *virtiofsProcess) Done() <-chan struct{} {
	return p.observeDone
}

func (p *virtiofsProcess) Result() (ProcessObservation, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.observed {
		return ProcessObservation{}, false
	}
	result := p.observation
	if result.ExitCode != nil {
		exitCode := *result.ExitCode
		result.ExitCode = &exitCode
	}
	return result, true
}

func (p *virtiofsProcess) storeObservation(wait processWaitResult) {
	p.mu.Lock()
	p.observation = ProcessObservation{
		PID:        p.handle.PID(),
		StartTime:  p.handle.StartTime(),
		Exited:     wait.Exited,
		Cause:      wait.Cause,
		ExitCode:   wait.ExitCode,
		Signal:     wait.Signal,
		ObservedAt: time.Now().UTC(),
	}
	p.observed = true
	p.mu.Unlock()
}

// markPrepared linearizes socket readiness with monitor completion. An exit
// after this check is still retained and observed by the sandbox watcher.
func (p *virtiofsProcess) markPrepared() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.observed {
		return nil
	}
	return observationError("virtiofsd exited before prepare completed", p.observation)
}

func observationError(message string, result ProcessObservation) error {
	detail := "process exit observed"
	if !result.Exited {
		detail = "process observer failed"
	}
	if result.Signal != "" {
		detail = fmt.Sprintf("signal %s", result.Signal)
	} else if result.ExitCode != nil {
		detail = fmt.Sprintf("exit code %d", *result.ExitCode)
	}
	if result.Cause != nil {
		return fmt.Errorf("%s (%s, pid %d): %w", message, detail, result.PID, result.Cause)
	}
	return fmt.Errorf("%s (%s, pid %d)", message, detail, result.PID)
}

// Close is strict-once. It never calls Wait: it signals through the bound
// handle, waits for the one monitor, and uses ConfirmExit only when observation
// did not prove process exit.
func (p *virtiofsProcess) Close() error {
	if p == nil || p.handle == nil {
		return nil
	}
	p.stopOnce.Do(func() {
		p.stopErr = p.close()
	})
	return p.stopErr
}

func (p *virtiofsProcess) close() error {
	var errs []error
	observation, observed := p.Result()
	if !observed || !observation.Exited {
		if err := p.handle.Kill(); err != nil {
			errs = append(errs, fmt.Errorf("kill virtiofsd pid %d: %w", p.handle.PID(), err))
		}
	}

	monitorDone := waitForDone(p.observeDone, processCloseTimeout)
	if monitorDone {
		observation, observed = p.Result()
	}
	if !observed || !observation.Exited {
		if err := p.handle.ConfirmExit(processCloseTimeout); err != nil {
			errs = append(errs, fmt.Errorf("confirm virtiofsd pid %d exit: %w", p.handle.PID(), err))
		}
		if !monitorDone {
			monitorDone = waitForDone(p.observeDone, processCloseTimeout)
		}
	}
	if !monitorDone {
		errs = append(errs, fmt.Errorf("virtiofsd pid %d monitor did not stop", p.handle.PID()))
	}
	if err := p.handle.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close virtiofsd pid %d handle: %w", p.handle.PID(), err))
	}
	return errors.Join(errs...)
}

func waitForDone(done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
