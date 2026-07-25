package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/creack/pty"
	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/pkg/ulog"
)

type managedProcess struct {
	mu         sync.Mutex
	cmd        *exec.Cmd
	tty        *os.File
	info       *pb.ProcessInfo
	dataEvents *processEventMultiplexer
	endEvents  *processEventMultiplexer
	endEvent   *pb.ProcessEvent
	droppedOut atomic.Int64
	outputDone chan struct{}
	tempScript string
}

type processEventSubscriber struct {
	ch   chan *pb.ProcessEvent
	done chan struct{}
	once sync.Once
}

func (s *processEventSubscriber) cancel() {
	s.once.Do(func() {
		close(s.done)
	})
}

func (s *processEventSubscriber) isCancelled() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

type processEventMultiplexer struct {
	Source chan *pb.ProcessEvent

	mu       sync.RWMutex
	channels []*processEventSubscriber
	exited   atomic.Bool
}

func newProcessEventMultiplexer(buffer int) *processEventMultiplexer {
	m := &processEventMultiplexer{
		Source: make(chan *pb.ProcessEvent, buffer),
	}
	go m.run()
	return m
}

func (m *processEventMultiplexer) run() {
	for event := range m.Source {
		m.mu.RLock()
		subs := append([]*processEventSubscriber(nil), m.channels...)
		m.mu.RUnlock()

		for _, sub := range subs {
			if sub.isCancelled() {
				continue
			}
			select {
			case sub.ch <- event:
			case <-sub.done:
			}
		}
	}

	m.exited.Store(true)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sub := range m.channels {
		sub.cancel()
		close(sub.ch)
	}
	m.channels = nil
}

func (m *processEventMultiplexer) Fork() (<-chan *pb.ProcessEvent, func()) {
	if m.exited.Load() {
		ch := make(chan *pb.ProcessEvent)
		close(ch)
		return ch, func() {}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exited.Load() {
		ch := make(chan *pb.ProcessEvent)
		close(ch)
		return ch, func() {}
	}

	sub := &processEventSubscriber{
		ch:   make(chan *pb.ProcessEvent),
		done: make(chan struct{}),
	}
	m.channels = append(m.channels, sub)

	return sub.ch, func() {
		m.remove(sub)
	}
}

func (m *processEventMultiplexer) remove(sub *processEventSubscriber) {
	sub.cancel()

	m.mu.Lock()
	defer m.mu.Unlock()
	for i, candidate := range m.channels {
		if candidate == sub {
			m.channels = append(m.channels[:i], m.channels[i+1:]...)
			return
		}
	}
}

type processOutputWriter struct {
	process *managedProcess
	stderr  bool
	pty     bool
}

func (w processOutputWriter) Write(p []byte) (int, error) {
	w.process.appendOutput(p, w.stderr, w.pty)
	return len(p), nil
}

type foregroundOutputState struct {
	started   chan struct{}
	startOnce sync.Once
	cancel    context.CancelFunc
	mu        sync.Mutex
	err       error
}

func newForegroundOutputState(cancel context.CancelFunc) *foregroundOutputState {
	return &foregroundOutputState{started: make(chan struct{}), cancel: cancel}
}

func (s *foregroundOutputState) markStarted() {
	s.startOnce.Do(func() {
		close(s.started)
	})
}

func (s *foregroundOutputState) failBeforeStart(err error) {
	s.setErr(err)
	s.markStarted()
}

func (s *foregroundOutputState) setErr(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

type foregroundOutputWriter struct {
	stream processConnectStream
	state  *foregroundOutputState
	stderr bool
	pty    bool
}

func (w foregroundOutputWriter) Write(p []byte) (int, error) {
	event := processDataEvent(p, w.stderr, w.pty)

	<-w.state.started
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	if w.state.err != nil {
		return 0, w.state.err
	}
	if err := w.stream.Send(event); err != nil {
		w.state.err = err
		w.state.cancel()
		return 0, err
	}
	return len(p), nil
}

func (s *foregroundOutputState) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func processStartEvent(pid int32) *pb.ProcessEvent {
	return &pb.ProcessEvent{
		Event: &pb.ProcessEvent_Start{
			Start: &pb.ProcessStartEvent{Pid: pid},
		},
	}
}

func processStdoutEvent(data []byte) *pb.ProcessEvent {
	return &pb.ProcessEvent{
		Event: &pb.ProcessEvent_Data{
			Data: &pb.ProcessDataEvent{
				Output: &pb.ProcessDataEvent_Stdout{Stdout: append([]byte(nil), data...)},
			},
		},
	}
}

func processStderrEvent(data []byte) *pb.ProcessEvent {
	return &pb.ProcessEvent{
		Event: &pb.ProcessEvent_Data{
			Data: &pb.ProcessDataEvent{
				Output: &pb.ProcessDataEvent_Stderr{Stderr: append([]byte(nil), data...)},
			},
		},
	}
}

func processPTYEvent(data []byte) *pb.ProcessEvent {
	return &pb.ProcessEvent{
		Event: &pb.ProcessEvent_Data{
			Data: &pb.ProcessDataEvent{
				Output: &pb.ProcessDataEvent_Pty{Pty: append([]byte(nil), data...)},
			},
		},
	}
}

func processDataEvent(data []byte, stderr, isPTY bool) *pb.ProcessEvent {
	switch {
	case isPTY:
		return processPTYEvent(data)
	case stderr:
		return processStderrEvent(data)
	default:
		return processStdoutEvent(data)
	}
}

func processEndEvent(exitCode int32, errMsg string) *pb.ProcessEvent {
	statusText := "exited"
	if errMsg != "" {
		statusText = "error"
	}
	return &pb.ProcessEvent{
		Event: &pb.ProcessEvent_End{
			End: &pb.ProcessEndEvent{
				ExitCode: exitCode,
				Exited:   true,
				Status:   statusText,
				Error:    errMsg,
			},
		},
	}
}

func (p *managedProcess) appendOutput(data []byte, stderr, isPTY bool) {
	event := processDataEvent(data, stderr, isPTY)
	select {
	case p.dataEvents.Source <- event:
	default:
		p.droppedOut.Add(1)
	}
}

func (p *managedProcess) finish(exitCode int32, errMsg string) {
	endEvent := processEndEvent(exitCode, errMsg)
	droppedOut := p.droppedOut.Load()

	p.mu.Lock()
	p.info.Running = false
	p.info.ExitCode = exitCode
	p.info.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	p.endEvent = endEvent
	pid := p.info.Pid
	tag := p.info.Tag
	p.mu.Unlock()

	close(p.dataEvents.Source)
	p.endEvents.Source <- endEvent
	close(p.endEvents.Source)
	if droppedOut > 0 {
		ulog.Warn("dropped process output events because stream subscribers were not draining",
			ulog.F("pid", pid),
			ulog.F("tag", tag),
			ulog.F("dropped", droppedOut))
	}
}

func (p *managedProcess) snapshot() *pb.ProcessInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneProcessInfo(p.info)
}

func cloneProcessInfo(info *pb.ProcessInfo) *pb.ProcessInfo {
	if info == nil {
		return nil
	}

	return &pb.ProcessInfo{
		Pid:        info.Pid,
		Tag:        info.Tag,
		Config:     cloneProcessConfig(info.Config),
		Running:    info.Running,
		StartedAt:  info.StartedAt,
		ExitCode:   info.ExitCode,
		FinishedAt: info.FinishedAt,
		Stdout:     info.Stdout,
		Stderr:     info.Stderr,
	}
}

func cloneProcessConfig(cfg *pb.ProcessConfig) *pb.ProcessConfig {
	if cfg == nil {
		return nil
	}

	return &pb.ProcessConfig{
		Cmd:  cfg.Cmd,
		Args: append([]string(nil), cfg.Args...),
		Env:  cloneEnv(cfg.Env),
		Cwd:  cfg.Cwd,
		Pty:  clonePTY(cfg.Pty),
	}
}

func clonePTY(ptyCfg *pb.PTY) *pb.PTY {
	if ptyCfg == nil {
		return nil
	}

	return &pb.PTY{
		Cols: ptyCfg.Cols,
		Rows: ptyCfg.Rows,
	}
}

func (p *managedProcess) finalEndEvent() *pb.ProcessEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endEvent
}

func cloneEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	clone := make(map[string]string, len(env))
	for k, v := range env {
		clone[k] = v
	}
	return clone
}

func (s *AgentServer) ensureProcessRegistryLocked() {
	if s.processes == nil {
		s.processes = make(map[int32]*managedProcess)
	}
	if s.processByTag == nil {
		s.processByTag = make(map[string]int32)
	}
}

func (s *AgentServer) removeProcess(process *managedProcess) {
	if process == nil || process.info == nil {
		return
	}
	pid := process.info.Pid
	tag := process.info.Tag

	s.processMu.Lock()
	defer s.processMu.Unlock()
	if s.processes != nil {
		delete(s.processes, pid)
	}
	if s.processByTag != nil && tag != "" && s.processByTag[tag] == pid {
		delete(s.processByTag, tag)
	}
}

func cleanupTempScript(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		ulog.Warn("failed to remove temporary script", ulog.F("path", path), ulog.F("error", err))
	}
}

func ptySize(cfg *pb.PTY) *pty.Winsize {
	size := &pty.Winsize{Cols: 80, Rows: 24}
	if cfg == nil {
		return size
	}
	if cfg.Cols > 0 {
		size.Cols = uint16(cfg.Cols)
	}
	if cfg.Rows > 0 {
		size.Rows = uint16(cfg.Rows)
	}
	return size
}

func sendForegroundEnd(stream processConnectStream, exitCode int32, errMsg string) error {
	return stream.Send(processEndEvent(exitCode, errMsg))
}

func (s *AgentServer) prepareWorkDir(cwd string) (string, error) {
	if cwd == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(homeDir, DirPerm); err != nil {
			return "", err
		}
		return homeDir, nil
	}
	if err := os.MkdirAll(cwd, DirPerm); err != nil {
		return "", err
	}
	return cwd, nil
}

// Write script file and return script path (if content exists).
func (s *AgentServer) writeScript(workDir, cmd, content string) (string, error) {
	if content == "" {
		return "", nil
	}

	scriptExtMap := map[string]string{
		"python":  "main.py",
		"python3": "main.py",
		"python2": "main.py",
		"node":    "main.js",
		"nodejs":  "main.js",
		"bash":    "main.sh",
		"sh":      "main.sh",
		"zsh":     "main.sh",
		"fish":    "main.sh",
		"lua":     "main.lua",
		"ruby":    "main.rb",
		"rb":      "main.rb",
	}

	scriptName := "conch-script-*.py"
	if name, ok := scriptExtMap[cmd]; ok {
		ext := filepath.Ext(name)
		scriptName = "conch-script-*" + ext
	}

	scriptFile, err := os.CreateTemp(workDir, scriptName)
	if err != nil {
		return "", err
	}
	scriptPath := scriptFile.Name()
	closed := false
	committed := false
	defer func() {
		if !closed {
			_ = scriptFile.Close()
		}
		if !committed {
			cleanupTempScript(scriptPath)
		}
	}()

	if _, err := scriptFile.Write([]byte(content)); err != nil {
		return "", err
	}
	if err := scriptFile.Close(); err != nil {
		closed = true
		return "", err
	}
	closed = true
	if err := os.Chmod(scriptPath, FilePerm); err != nil {
		return "", err
	}

	committed = true
	return scriptPath, nil
}

func applyCommandEnvAndDir(cmd *exec.Cmd, workDir string, envMap map[string]string) {
	if workDir != "" {
		cmd.Dir = workDir
	}

	env := os.Environ()
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
}

// Execute command and stream output events while it is running.
func (s *AgentServer) executeCmd(ctx context.Context, cmdName string, args []string, workDir string, envMap map[string]string, ptyConfig *pb.PTY, stream processConnectStream) (int, bool, error) {
	if cmdName == "" {
		return -1, false, fmt.Errorf("command is required")
	}

	cmdCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, cmdName, args...)
	applyCommandEnvAndDir(cmd, workDir, envMap)
	if ptyConfig != nil {
		return executePtyCmd(cmdCtx, cmd, ptyConfig, stream, cancel)
	}

	outputState := newForegroundOutputState(cancel)
	cmd.Stdout = foregroundOutputWriter{stream: stream, state: outputState}
	cmd.Stderr = foregroundOutputWriter{stream: stream, state: outputState, stderr: true}

	if err := cmd.Start(); err != nil {
		return -1, false, err
	}
	if err := stream.Send(processStartEvent(int32(cmd.Process.Pid))); err != nil {
		outputState.failBeforeStart(err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return exitCode, true, err
	}
	outputState.markStarted()

	waitErr := cmd.Wait()

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	if err := outputState.Err(); err != nil {
		return exitCode, true, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return exitCode, false, ctxErr
	}
	if err := signaledProcessError(cmd.ProcessState); err != nil {
		return exitCode, false, err
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitCode, false, nil
		}
		return exitCode, false, waitErr
	}

	return exitCode, false, nil
}

func executePtyCmd(ctx context.Context, cmd *exec.Cmd, ptyConfig *pb.PTY, stream processConnectStream, cancel context.CancelFunc) (int, bool, error) {
	tty, err := pty.StartWithSize(cmd, ptySize(ptyConfig))
	if err != nil {
		return -1, false, err
	}
	defer tty.Close()

	outputState := newForegroundOutputState(cancel)
	readDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(foregroundOutputWriter{stream: stream, state: outputState, pty: true}, tty)
		if errors.Is(copyErr, os.ErrClosed) || errors.Is(copyErr, syscall.EIO) {
			copyErr = nil
		}
		readDone <- copyErr
	}()

	if err := stream.Send(processStartEvent(int32(cmd.Process.Pid))); err != nil {
		outputState.failBeforeStart(err)
		_ = cmd.Process.Kill()
		_ = tty.Close()
		_ = cmd.Wait()
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return exitCode, true, err
	}
	outputState.markStarted()

	waitErr := cmd.Wait()
	var readErr error
	select {
	case readErr = <-readDone:
	case <-time.After(500 * time.Millisecond):
		_ = tty.Close()
		readErr = <-readDone
	}

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if err := outputState.Err(); err != nil {
		return exitCode, true, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return exitCode, false, ctxErr
	}
	if err := signaledProcessError(cmd.ProcessState); err != nil {
		return exitCode, false, err
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return exitCode, false, waitErr
		}
	}
	if readErr != nil {
		return exitCode, false, readErr
	}
	return exitCode, false, nil
}

func signaledProcessError(state *os.ProcessState) error {
	if state == nil {
		return nil
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return nil
	}
	return fmt.Errorf("process terminated by signal %s", status.Signal())
}

// Starts a process with custom working dir, environment, and script content.
func (s *AgentServer) StartProcess(ctx context.Context, req *pb.StartProcessRequest, stream processConnectStream) error {
	if req == nil {
		return connectError(connect.CodeInvalidArgument, "start process request is required")
	}

	ulog.Info("Received start process request",
		ulog.F("cmd", req.Cmd),
		ulog.F("args", fmt.Sprintf("%v", req.Args)),
		ulog.F("cwd", req.Cwd),
		ulog.F("has_content", req.Content != ""))

	if req.Content != "" && len(req.Args) > 0 {
		return connectError(connect.CodeInvalidArgument, "content and args cannot both be set")
	}

	// Prepare work dir
	workDir, err := s.prepareWorkDir(req.Cwd)
	if err != nil {
		errMsg := "failed to prepare working directory: " + err.Error()
		return connectError(connect.CodeInvalidArgument, errMsg)
	}

	// Write script file
	scriptPath, err := s.writeScript(workDir, req.Cmd, req.Content)
	if err != nil {
		errMsg := "failed to write script file: " + err.Error()
		return connectError(connect.CodeInternal, errMsg)
	}
	if scriptPath != "" {
		if !req.Background {
			defer cleanupTempScript(scriptPath)
		}
	}

	// Build command args
	args := req.Args
	if len(args) == 0 && scriptPath != "" {
		args = []string{scriptPath}
	}

	if req.Background {
		return s.startBackgroundProcess(req, args, workDir, scriptPath, stream)
	}

	// Execute command
	exitCode, streamFailed, err := s.executeCmd(ctx, req.Cmd, args, workDir, req.Env, req.Pty, stream)
	errMsg := ""
	if err != nil {
		if streamFailed {
			return err
		}
		return connectErrorf(connect.CodeInvalidArgument, "failed to execute process: %v", err)
	}
	return sendForegroundEnd(stream, int32(exitCode), errMsg)
}

func (s *AgentServer) startBackgroundProcess(req *pb.StartProcessRequest, args []string, workDir, scriptPath string, stream processConnectStream) error {
	if req.Cmd == "" {
		cleanupTempScript(scriptPath)
		return connectError(connect.CodeInvalidArgument, "command is required")
	}

	cmd := exec.Command(req.Cmd, args...)
	applyCommandEnvAndDir(cmd, workDir, req.Env)

	process := &managedProcess{
		cmd: cmd,
		info: &pb.ProcessInfo{
			Tag: req.Tag,
			Config: &pb.ProcessConfig{
				Cmd:  req.Cmd,
				Args: append([]string(nil), args...),
				Env:  cloneEnv(req.Env),
				Cwd:  workDir,
				Pty:  req.Pty,
			},
			Running:   true,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
			ExitCode:  -1,
		},
		dataEvents: newProcessEventMultiplexer(64),
		endEvents:  newProcessEventMultiplexer(1),
		tempScript: scriptPath,
	}
	if req.Pty == nil {
		cmd.Stdout = processOutputWriter{process: process}
		cmd.Stderr = processOutputWriter{process: process, stderr: true}
	}

	data, dataCancel := process.dataEvents.Fork()
	end, endCancel := process.endEvents.Fork()

	if req.Tag != "" {
		s.processMu.Lock()
		s.ensureProcessRegistryLocked()
		if existingPID, ok := s.processByTag[req.Tag]; ok {
			if existing := s.processes[existingPID]; existing != nil && existing.snapshot().Running {
				s.processMu.Unlock()
				dataCancel()
				endCancel()
				cleanupTempScript(scriptPath)
				return connectError(connect.CodeAlreadyExists, "background process tag already exists: "+req.Tag)
			}
		}
		if err := startManagedProcess(process, req.Pty); err != nil {
			s.processMu.Unlock()
			dataCancel()
			endCancel()
			cleanupTempScript(scriptPath)
			return connectErrorf(connect.CodeInvalidArgument, "failed to start background process: %v", err)
		}
		process.info.Pid = int32(cmd.Process.Pid)
		s.processes[process.info.Pid] = process
		s.processByTag[req.Tag] = process.info.Pid
		s.processMu.Unlock()

		go s.waitManagedProcess(process)
		return s.streamManagedProcessEvents(process, data, dataCancel, end, endCancel, stream)
	}

	if err := startManagedProcess(process, req.Pty); err != nil {
		dataCancel()
		endCancel()
		cleanupTempScript(scriptPath)
		return connectErrorf(connect.CodeInvalidArgument, "failed to start background process: %v", err)
	}
	process.info.Pid = int32(cmd.Process.Pid)

	s.processMu.Lock()
	s.ensureProcessRegistryLocked()
	s.processes[process.info.Pid] = process
	s.processMu.Unlock()

	go s.waitManagedProcess(process)

	return s.streamManagedProcessEvents(process, data, dataCancel, end, endCancel, stream)
}

func startManagedProcess(process *managedProcess, ptyConfig *pb.PTY) error {
	if ptyConfig == nil {
		return process.cmd.Start()
	}

	tty, err := pty.StartWithSize(process.cmd, ptySize(ptyConfig))
	if err != nil {
		return err
	}
	process.tty = tty
	process.outputDone = make(chan struct{})

	go func() {
		defer close(process.outputDone)
		defer tty.Close()
		buf := make([]byte, 16*1024)
		for {
			n, readErr := tty.Read(buf)
			if n > 0 {
				process.appendOutput(buf[:n], false, true)
			}
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, os.ErrClosed) || errors.Is(readErr, syscall.EIO) {
				return
			}
			if readErr != nil {
				ulog.Warn("failed to read process pty", ulog.F("error", readErr))
				return
			}
		}
	}()
	return nil
}

func (s *AgentServer) waitManagedProcess(p *managedProcess) {
	waitErr := p.cmd.Wait()
	if p.outputDone != nil {
		select {
		case <-p.outputDone:
		case <-time.After(500 * time.Millisecond):
			ulog.Warn("timed out waiting for process output reader",
				ulog.F("pid", p.info.Pid),
				ulog.F("tag", p.info.Tag))
			if p.tty != nil {
				_ = p.tty.Close()
			}
			<-p.outputDone
		}
	}
	if p.tty != nil {
		_ = p.tty.Close()
	}
	exitCode := int32(-1)
	if p.cmd.ProcessState != nil {
		exitCode = int32(p.cmd.ProcessState.ExitCode())
	}

	errMsg := ""
	if err := signaledProcessError(p.cmd.ProcessState); err != nil {
		errMsg = err.Error()
	} else if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			errMsg = waitErr.Error()
		}
	}
	if p.tempScript != "" {
		cleanupTempScript(p.tempScript)
	}
	s.removeProcess(p)
	p.finish(exitCode, errMsg)
}

func (s *AgentServer) findProcess(selector *pb.ProcessSelector) (*managedProcess, error) {
	if selector == nil || selector.Pid == 0 && selector.Tag == "" {
		return nil, connectError(connect.CodeInvalidArgument, "process pid or tag is required")
	}

	s.processMu.Lock()
	defer s.processMu.Unlock()
	s.ensureProcessRegistryLocked()

	if selector.Pid != 0 {
		process := s.processes[selector.Pid]
		if process == nil {
			return nil, connectErrorf(connect.CodeNotFound, "process pid %d not found", selector.Pid)
		}
		return process, nil
	}

	pid, ok := s.processByTag[selector.Tag]
	if !ok {
		return nil, connectErrorf(connect.CodeNotFound, "process tag %q not found", selector.Tag)
	}
	process := s.processes[pid]
	if process == nil {
		return nil, connectErrorf(connect.CodeNotFound, "process tag %q not found", selector.Tag)
	}
	return process, nil
}

func (s *AgentServer) Connect(req *pb.ConnectProcessRequest, stream processConnectStream) error {
	selector := req.GetProcess()
	ulog.Info("Received connect process request",
		ulog.F("pid", selector.GetPid()),
		ulog.F("tag", selector.GetTag()))

	process, err := s.findProcess(selector)
	if err != nil {
		return err
	}

	return s.streamManagedProcess(process, stream)
}

func (s *AgentServer) streamManagedProcess(process *managedProcess, stream processConnectStream) error {
	data, dataCancel := process.dataEvents.Fork()
	end, endCancel := process.endEvents.Fork()
	return s.streamManagedProcessEvents(process, data, dataCancel, end, endCancel, stream)
}

func (s *AgentServer) streamManagedProcessEvents(
	process *managedProcess,
	data <-chan *pb.ProcessEvent,
	dataCancel func(),
	end <-chan *pb.ProcessEvent,
	endCancel func(),
	stream processConnectStream,
) error {
	defer dataCancel()
	defer endCancel()

	process.mu.Lock()
	pid := process.info.Pid
	tag := process.info.Tag
	process.mu.Unlock()

	ulog.Info("Connected to background process",
		ulog.F("pid", pid),
		ulog.F("tag", tag))

	if err := stream.Send(processStartEvent(pid)); err != nil {
		return err
	}

dataLoop:
	for {
		select {
		case event, ok := <-data:
			if !ok {
				break dataLoop
			}
			if err := stream.Send(event); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}

	select {
	case event, ok := <-end:
		if !ok {
			if event := process.finalEndEvent(); event != nil {
				return stream.Send(event)
			}
			return connectError(connect.CodeInternal, "process end event channel closed before sending end event")
		}
		return stream.Send(event)
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

func (s *AgentServer) List(ctx context.Context, req *pb.ListProcessesRequest) (*pb.ListProcessesResponse, error) {
	ulog.Info("Received list processes request")

	s.processMu.Lock()
	defer s.processMu.Unlock()
	s.ensureProcessRegistryLocked()

	resp := &pb.ListProcessesResponse{}
	for _, process := range s.processes {
		info := process.snapshot()
		resp.Processes = append(resp.Processes, info)
	}
	ulog.Info("Listed background processes",
		ulog.F("count", len(resp.Processes)))
	return resp, nil
}

func (s *AgentServer) SendSignal(ctx context.Context, req *pb.SendSignalRequest) (*pb.SendSignalResponse, error) {
	if req == nil {
		return nil, connectError(connect.CodeInvalidArgument, "send signal request is required")
	}

	selector := req.GetProcess()
	ulog.Info("Received send signal request",
		ulog.F("pid", selector.GetPid()),
		ulog.F("tag", selector.GetTag()),
		ulog.F("signal", req.GetSignal()))

	process, err := s.findProcess(selector)
	if err != nil {
		return nil, err
	}

	sig := req.GetSignal()
	if sig == 0 {
		return nil, connectError(connect.CodeInvalidArgument, "signal must not be zero")
	}

	process.mu.Lock()
	running := process.info.Running
	osProcess := process.cmd.Process
	pid := process.info.Pid
	tag := process.info.Tag
	process.mu.Unlock()

	if !running || osProcess == nil {
		return nil, connectError(connect.CodeFailedPrecondition, "process is not running")
	}
	if err := osProcess.Signal(syscall.Signal(sig)); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return nil, connectErrorf(connect.CodeFailedPrecondition, "process is not running: %v", err)
		}
		return nil, connectErrorf(connect.CodeInternal, "failed to signal process: %v", err)
	}
	ulog.Info("Sent signal to background process",
		ulog.F("pid", pid),
		ulog.F("tag", tag),
		ulog.F("signal", sig))
	return &pb.SendSignalResponse{}, nil
}
