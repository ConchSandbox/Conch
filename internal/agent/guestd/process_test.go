package guestd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/openeuler/Conch/api/go_proto"
)

func TestStartProcessRejectsNilRequest(t *testing.T) {
	server := &AgentServer{}
	err := startProcessErrorForTest(t, server, nil)
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "request is required") {
		t.Fatalf("StartProcess(nil) error = %v, want InvalidArgument request error", err)
	}
}

func TestStartProcessReturnsExitCodeForNonZeroExit(t *testing.T) {
	server := &AgentServer{}
	resp := startProcessForTest(t, server, &pb.StartProcessRequest{
		Cmd:  "sh",
		Args: []string{"-c", "echo out; echo err >&2; exit 7"},
		Cwd:  t.TempDir(),
	})
	if resp.Error != "" {
		t.Fatalf("StartProcess() response error = %q, want empty", resp.Error)
	}
	if resp.ExitCode != 7 {
		t.Fatalf("StartProcess() exit code = %d, want 7", resp.ExitCode)
	}
	if !strings.Contains(resp.Stdout, "out") {
		t.Fatalf("StartProcess() stdout = %q, want out", resp.Stdout)
	}
	if !strings.Contains(resp.Stderr, "err") {
		t.Fatalf("StartProcess() stderr = %q, want err", resp.Stderr)
	}
}

func TestStartProcessStreamsForegroundOutputBeforeExit(t *testing.T) {
	server := &AgentServer{}
	workDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &blockingFirstDataStream{
		ctx:       ctx,
		firstData: make(chan struct{}),
		release:   make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		done <- server.StartProcess(ctx, &pb.StartProcessRequest{
			Cmd:  "sh",
			Args: []string{"-c", "printf stream-first; while [ ! -f gate ]; do sleep 0.05; done; printf stream-second"},
			Cwd:  workDir,
		}, stream)
	}()

	select {
	case <-stream.firstData:
	case err := <-done:
		t.Fatalf("StartProcess() returned before streaming first output: %v", err)
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("StartProcess() did not stream foreground output before command exit")
	}

	select {
	case err := <-done:
		t.Fatalf("StartProcess() returned before command was unblocked: %v", err)
	default:
	}

	if _, err := os.Stat(filepath.Join(workDir, "gate")); !os.IsNotExist(err) {
		t.Fatalf("gate file stat error = %v, want not exist", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "gate"), []byte("go"), 0o600); err != nil {
		t.Fatalf("failed to create gate file: %v", err)
	}
	close(stream.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StartProcess() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("StartProcess() did not finish after command was unblocked")
	}

	events := stream.Events()
	if len(events) == 0 || events[0].GetStart() == nil {
		t.Fatalf("StartProcess() first event = %+v, want start", events)
	}
	resp := responseFromProcessEvents(&pb.StartProcessRequest{Cmd: "sh"}, events)
	if resp.ExitCode != 0 {
		t.Fatalf("StartProcess() exit code = %d, want 0", resp.ExitCode)
	}
	if !strings.Contains(resp.Stdout, "stream-first") || !strings.Contains(resp.Stdout, "stream-second") {
		t.Fatalf("StartProcess() stdout = %q, want both streamed chunks", resp.Stdout)
	}
}

func TestStartProcessCancelsForegroundCommandWhenDataSendFails(t *testing.T) {
	server := &AgentServer{}
	workDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamErr := errors.New("stream send failed")
	done := make(chan error, 1)
	go func() {
		done <- server.StartProcess(ctx, &pb.StartProcessRequest{
			Cmd:  "sh",
			Args: []string{"-c", "printf before-fail; while [ ! -f gate ]; do sleep 0.05; done"},
			Cwd:  workDir,
		}, &failDataStream{ctx: ctx, err: streamErr})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, streamErr) {
			t.Fatalf("StartProcess() error = %v, want stream error", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("StartProcess() did not return after foreground data send failed")
	}

	if _, err := os.Stat(filepath.Join(workDir, "gate")); !os.IsNotExist(err) {
		t.Fatalf("gate file stat error = %v, want not exist", err)
	}
}

func TestStartProcessRemovesTemporaryScript(t *testing.T) {
	server := &AgentServer{}
	workDir := t.TempDir()

	resp := startProcessForTest(t, server, &pb.StartProcessRequest{
		Cmd:     "sh",
		Cwd:     workDir,
		Content: "echo script-ran",
	})
	if resp.Error != "" {
		t.Fatalf("StartProcess() response error = %q, want empty", resp.Error)
	}
	if !strings.Contains(resp.Stdout, "script-ran") {
		t.Fatalf("StartProcess() stdout = %q, want script-ran", resp.Stdout)
	}

	matches, err := filepath.Glob(filepath.Join(workDir, "conch-script-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary scripts remain after StartProcess: %v", matches)
	}
}

func TestStartProcessRejectsContentWithArgs(t *testing.T) {
	server := &AgentServer{}
	workDir := t.TempDir()

	err := startProcessErrorForTest(t, server, &pb.StartProcessRequest{
		Cmd:     "sh",
		Args:    []string{"-c", "echo args-ran"},
		Cwd:     workDir,
		Content: "echo content-ran",
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "content and args cannot both be set") {
		t.Fatalf("StartProcess(content with args) error = %v, want InvalidArgument validation error", err)
	}

	matches, err := filepath.Glob(filepath.Join(workDir, "conch-script-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary scripts created after content with args rejection: %v", matches)
	}
}

func TestStartProcessRunsExistingFileFromArgs(t *testing.T) {
	server := &AgentServer{}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "main.sh"), []byte("echo uploaded-file-ran"), FilePerm); err != nil {
		t.Fatalf("WriteFile(main.sh) error = %v", err)
	}

	resp := startProcessForTest(t, server, &pb.StartProcessRequest{
		Cmd:  "sh",
		Cwd:  workDir,
		Args: []string{"main.sh"},
	})
	if resp.Error != "" {
		t.Fatalf("StartProcess() response error = %q, want empty", resp.Error)
	}
	if !strings.Contains(resp.Stdout, "uploaded-file-ran") {
		t.Fatalf("StartProcess() stdout = %q, want uploaded-file-ran", resp.Stdout)
	}
}

func TestStartProcessSupportsPTY(t *testing.T) {
	server := &AgentServer{}
	resp := startProcessForTest(t, server, &pb.StartProcessRequest{
		Cmd: "sh",
		Args: []string{
			"-c",
			"printf pty-ready",
		},
		Cwd: t.TempDir(),
		Pty: &pb.PTY{Cols: 80, Rows: 24},
	})
	if resp.Error != "" {
		t.Fatalf("StartProcess(pty) response error = %q, want empty", resp.Error)
	}
	if !strings.Contains(resp.Stdout, "pty-ready") {
		t.Fatalf("StartProcess(pty) stdout = %q, want pty-ready", resp.Stdout)
	}
}

func TestStartProcessCancelsForegroundPTYCommandWhenDataSendFails(t *testing.T) {
	server := &AgentServer{}
	workDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamErr := errors.New("pty stream send failed")
	done := make(chan error, 1)
	go func() {
		done <- server.StartProcess(ctx, &pb.StartProcessRequest{
			Cmd:  "sh",
			Args: []string{"-c", "printf pty-before-fail; while [ ! -f gate ]; do sleep 0.05; done"},
			Cwd:  workDir,
			Pty:  &pb.PTY{Cols: 80, Rows: 24},
		}, &failDataStream{ctx: ctx, err: streamErr})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, streamErr) {
			t.Fatalf("StartProcess(pty) error = %v, want stream error", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("StartProcess(pty) did not return after foreground data send failed")
	}

	if _, err := os.Stat(filepath.Join(workDir, "gate")); !os.IsNotExist(err) {
		t.Fatalf("gate file stat error = %v, want not exist", err)
	}
}

func TestBackgroundProcessLifecycle(t *testing.T) {
	server := &AgentServer{}
	bg := startBackgroundProcessForTest(t, server, &pb.StartProcessRequest{
		Cmd:        "sleep",
		Args:       []string{"5"},
		Cwd:        t.TempDir(),
		Background: true,
		Tag:        "sleepy",
	})
	resp := bg.resp
	if resp.Error != "" {
		t.Fatalf("StartProcess(background) response error = %q, want empty", resp.Error)
	}
	if resp.Process == nil || !resp.Process.Running || resp.Process.Tag != "sleepy" || resp.Process.Pid == 0 {
		t.Fatalf("background process = %+v, want running tagged process", resp.Process)
	}

	list, err := server.List(context.Background(), &pb.ListProcessesRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.Processes) != 1 || list.Processes[0].Tag != "sleepy" {
		t.Fatalf("List() = %+v, want sleepy process", list.Processes)
	}

	if _, err := server.SendSignal(context.Background(), &pb.SendSignalRequest{
		Process: &pb.ProcessSelector{Tag: "sleepy"},
		Signal:  15,
	}); err != nil {
		t.Fatalf("SendSignal() error = %v", err)
	}
	waitForProcessExit(t, server, "sleepy")
}

func TestBackgroundProcessRejectsDuplicateRunningTag(t *testing.T) {
	server := &AgentServer{}
	bg := startBackgroundProcessForTest(t, server, &pb.StartProcessRequest{
		Cmd:        "sleep",
		Args:       []string{"5"},
		Cwd:        t.TempDir(),
		Background: true,
		Tag:        "duplicate",
	})
	resp := bg.resp
	if resp.Error != "" {
		t.Fatalf("StartProcess(background) response error = %q, want empty", resp.Error)
	}
	t.Cleanup(func() {
		_, _ = server.SendSignal(context.Background(), &pb.SendSignalRequest{
			Process: &pb.ProcessSelector{Tag: "duplicate"},
			Signal:  9,
		})
	})

	duplicateWorkDir := t.TempDir()
	err := startProcessErrorForTest(t, server, &pb.StartProcessRequest{
		Cmd:        "sh",
		Cwd:        duplicateWorkDir,
		Content:    "echo should-not-run",
		Background: true,
		Tag:        "duplicate",
	})
	if connect.CodeOf(err) != connect.CodeAlreadyExists || !strings.Contains(err.Error(), "tag already exists") {
		t.Fatalf("StartProcess(duplicate tag) error = %v, want AlreadyExists duplicate tag error", err)
	}

	matches, err := filepath.Glob(filepath.Join(duplicateWorkDir, "conch-script-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary scripts remain after duplicate tag rejection: %v", matches)
	}
}

func TestBackgroundProcessCleansTemporaryScriptWhenCommandMissing(t *testing.T) {
	server := &AgentServer{}
	workDir := t.TempDir()

	err := startProcessErrorForTest(t, server, &pb.StartProcessRequest{
		Cwd:        workDir,
		Content:    "echo should-not-run",
		Background: true,
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("StartProcess(background missing cmd) error = %v, want InvalidArgument command error", err)
	}

	matches, err := filepath.Glob(filepath.Join(workDir, "conch-script-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary scripts remain after background command validation: %v", matches)
	}
}

func TestSendSignalRejectsNilRequest(t *testing.T) {
	server := &AgentServer{}
	_, err := server.SendSignal(context.Background(), nil)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("SendSignal(nil) error = %v, want InvalidArgument", err)
	}
}

func TestSendSignalRejectsZeroSignal(t *testing.T) {
	server := &AgentServer{}
	bg := startBackgroundProcessForTest(t, server, &pb.StartProcessRequest{
		Cmd:        "sleep",
		Args:       []string{"5"},
		Cwd:        t.TempDir(),
		Background: true,
		Tag:        "zero-signal",
	})
	resp := bg.resp
	if resp.Error != "" {
		t.Fatalf("StartProcess(background) response error = %q, want empty", resp.Error)
	}
	t.Cleanup(func() {
		_, _ = server.SendSignal(context.Background(), &pb.SendSignalRequest{
			Process: &pb.ProcessSelector{Tag: "zero-signal"},
			Signal:  9,
		})
	})

	_, err := server.SendSignal(context.Background(), &pb.SendSignalRequest{
		Process: &pb.ProcessSelector{Tag: "zero-signal"},
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("SendSignal(signal=0) error = %v, want InvalidArgument", err)
	}

	list, err := server.List(context.Background(), &pb.ListProcessesRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.Processes) != 1 || list.Processes[0].Tag != "zero-signal" || !list.Processes[0].Running {
		t.Fatalf("process after signal=0 = %+v, want still running zero-signal process", list.Processes)
	}
}

func TestSendSignalMapsExitedProcessRaceToFailedPrecondition(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	pid := int32(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	server := &AgentServer{
		processes: map[int32]*managedProcess{
			pid: {
				cmd: cmd,
				info: &pb.ProcessInfo{
					Pid:     pid,
					Running: true,
				},
			},
		},
	}

	_, err := server.SendSignal(context.Background(), &pb.SendSignalRequest{
		Process: &pb.ProcessSelector{Pid: pid},
		Signal:  15,
	})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("SendSignal(exited process) error = %v, want FailedPrecondition", err)
	}
}

type fakeProcessConnectStream struct {
	ctx     context.Context
	mu      sync.Mutex
	events  []*pb.ProcessEvent
	startCh chan *pb.ProcessStartEvent
}

func (s *fakeProcessConnectStream) Send(event *pb.ProcessEvent) error {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	if start := event.GetStart(); start != nil && s.startCh != nil {
		select {
		case s.startCh <- start:
		default:
		}
	}
	return nil
}

func (s *fakeProcessConnectStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *fakeProcessConnectStream) Events() []*pb.ProcessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.ProcessEvent(nil), s.events...)
}

type blockingFirstDataStream struct {
	ctx       context.Context
	mu        sync.Mutex
	events    []*pb.ProcessEvent
	firstData chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *blockingFirstDataStream) Send(event *pb.ProcessEvent) error {
	if event.GetData() != nil {
		s.once.Do(func() {
			close(s.firstData)
		})
		select {
		case <-s.release:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}

	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	return nil
}

func (s *blockingFirstDataStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *blockingFirstDataStream) Events() []*pb.ProcessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.ProcessEvent(nil), s.events...)
}

type failDataStream struct {
	ctx context.Context
	err error
}

func (s *failDataStream) Send(event *pb.ProcessEvent) error {
	if event.GetData() != nil {
		return s.err
	}
	return nil
}

func (s *failDataStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

type backgroundStart struct {
	resp   *testProcessEventResult
	cancel context.CancelFunc
	done   chan error
}

type testProcessEventResult struct {
	Stdout   string
	Stderr   string
	ExitCode int32
	Error    string
	Process  *pb.ProcessInfo
}

func startProcessForTest(t *testing.T, server *AgentServer, req *pb.StartProcessRequest) *testProcessEventResult {
	t.Helper()
	stream := &fakeProcessConnectStream{}
	if err := server.StartProcess(context.Background(), req, stream); err != nil {
		t.Fatalf("StartProcess() error = %v", err)
	}
	return responseFromProcessEvents(req, stream.Events())
}

func startProcessErrorForTest(t *testing.T, server *AgentServer, req *pb.StartProcessRequest) error {
	t.Helper()
	stream := &fakeProcessConnectStream{}
	err := server.StartProcess(context.Background(), req, stream)
	if err == nil {
		t.Fatal("StartProcess() error = nil, want error")
	}
	if events := stream.Events(); len(events) != 0 {
		t.Fatalf("StartProcess() streamed events on startup error: %+v", events)
	}
	return err
}

func startBackgroundProcessForTest(t *testing.T, server *AgentServer, req *pb.StartProcessRequest) *backgroundStart {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeProcessConnectStream{ctx: ctx, startCh: make(chan *pb.ProcessStartEvent, 1)}
	done := make(chan error, 1)
	go func() {
		done <- server.StartProcess(ctx, req, stream)
	}()

	select {
	case <-stream.startCh:
	case err := <-done:
		if err != nil {
			t.Fatalf("StartProcess(background) error = %v", err)
		}
		resp := responseFromProcessEvents(req, stream.Events())
		t.Fatalf("StartProcess(background) ended before start event: %+v", resp)
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("StartProcess(background) did not send start event before deadline")
	}

	bg := &backgroundStart{
		resp:   responseFromProcessEvents(req, stream.Events()),
		cancel: cancel,
		done:   done,
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	return bg
}

func responseFromProcessEvents(req *pb.StartProcessRequest, events []*pb.ProcessEvent) *testProcessEventResult {
	resp := &testProcessEventResult{ExitCode: -1}
	for _, event := range events {
		if start := event.GetStart(); start != nil {
			resp.Process = &pb.ProcessInfo{
				Pid:       start.Pid,
				Tag:       req.GetTag(),
				Running:   true,
				ExitCode:  -1,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
				Config: &pb.ProcessConfig{
					Cmd:  req.GetCmd(),
					Args: append([]string(nil), req.GetArgs()...),
					Env:  cloneEnv(req.GetEnv()),
					Cwd:  req.GetCwd(),
					Pty:  req.GetPty(),
				},
			}
		}
		if data := event.GetData(); data != nil {
			resp.Stdout += data.GetStdout()
			resp.Stdout += data.GetPty()
			resp.Stderr += data.GetStderr()
		}
		if end := event.GetEnd(); end != nil {
			resp.ExitCode = end.ExitCode
			resp.Error = end.Error
			if resp.Process != nil {
				resp.Process.Running = false
				resp.Process.ExitCode = end.ExitCode
				resp.Process.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			}
		}
	}
	return resp
}

func TestConnectBackgroundProcessStreamsOutput(t *testing.T) {
	server := &AgentServer{}
	startBackgroundProcessForTest(t, server, &pb.StartProcessRequest{
		Cmd:        "sh",
		Args:       []string{"-c", "sleep 1; echo stream-ready"},
		Cwd:        t.TempDir(),
		Background: true,
		Tag:        "streamer",
	})

	stream := &fakeProcessConnectStream{}
	if err := server.Connect(&pb.ConnectProcessRequest{Process: &pb.ProcessSelector{Tag: "streamer"}}, stream); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	var stdout strings.Builder
	exited := false
	for _, event := range stream.events {
		if data := event.GetData(); data != nil {
			stdout.WriteString(data.GetStdout())
		}
		if event.GetEnd() != nil {
			exited = true
		}
	}
	if !strings.Contains(stdout.String(), "stream-ready") {
		t.Fatalf("Connect() stdout = %q, want stream-ready", stdout.String())
	}
	if !exited {
		t.Fatal("Connect() did not receive exit event")
	}
}

func TestConnectStreamsEndAfterLargeOutput(t *testing.T) {
	server := &AgentServer{}
	startBackgroundProcessForTest(t, server, &pb.StartProcessRequest{
		Cmd:        "sh",
		Args:       []string{"-c", "sleep 1; for i in $(seq 1 200); do echo line-$i; done"},
		Cwd:        t.TempDir(),
		Background: true,
		Tag:        "large-streamer",
	})

	stream := &fakeProcessConnectStream{}
	if err := server.Connect(&pb.ConnectProcessRequest{Process: &pb.ProcessSelector{Tag: "large-streamer"}}, stream); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	var stdout strings.Builder
	exited := false
	for _, event := range stream.events {
		if data := event.GetData(); data != nil {
			stdout.WriteString(data.GetStdout())
		}
		if event.GetEnd() != nil {
			exited = true
		}
	}
	if !strings.Contains(stdout.String(), "line-200") {
		t.Fatalf("Connect() stdout missing line-200, got %q", stdout.String())
	}
	if !exited {
		t.Fatal("Connect() did not receive end event after large output")
	}
}

func TestConnectUsesFinalEndEventWhenEndSubscriberMissesEvent(t *testing.T) {
	process := &managedProcess{
		info: &pb.ProcessInfo{
			Pid:     4242,
			Running: false,
		},
		dataEvents: newProcessEventMultiplexer(1),
		endEvents:  newProcessEventMultiplexer(1),
		endEvent:   processEndEvent(0, ""),
	}
	close(process.dataEvents.Source)
	close(process.endEvents.Source)

	deadline := time.Now().Add(time.Second)
	for (!process.dataEvents.exited.Load() || !process.endEvents.exited.Load()) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !process.dataEvents.exited.Load() || !process.endEvents.exited.Load() {
		t.Fatal("multiplexers did not close before deadline")
	}

	server := &AgentServer{
		processes: map[int32]*managedProcess{
			4242: process,
		},
	}
	stream := &fakeProcessConnectStream{}
	if err := server.Connect(&pb.ConnectProcessRequest{Process: &pb.ProcessSelector{Pid: 4242}}, stream); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	if len(stream.events) != 2 {
		t.Fatalf("Connect() events = %+v, want start and end", stream.events)
	}
	if stream.events[0].GetStart().GetPid() != 4242 {
		t.Fatalf("Connect() start = %+v, want pid 4242", stream.events[0])
	}
	if end := stream.events[1].GetEnd(); end == nil || end.ExitCode != 0 || !end.Exited {
		t.Fatalf("Connect() end = %+v, want successful end event", stream.events[1])
	}
}

func TestBackgroundProcessDoesNotBlockWhenStartStreamIsNotDrained(t *testing.T) {
	server := &AgentServer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &blockingAfterStartStream{
		ctx:     ctx,
		startCh: make(chan struct{}, 1),
	}
	done := make(chan error, 1)
	go func() {
		done <- server.StartProcess(ctx, &pb.StartProcessRequest{
			Cmd: "sh",
			Args: []string{
				"-c",
				"head -c 7340032 /dev/zero | tr '\\0' x",
			},
			Cwd:        t.TempDir(),
			Background: true,
			Tag:        "undrained-output",
		}, stream)
	}()

	select {
	case <-stream.startCh:
	case err := <-done:
		t.Fatalf("StartProcess(background) returned before start event: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("StartProcess(background) did not send start event before deadline")
	}

	waitForProcessExit(t, server, "undrained-output")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StartProcess(background) stream did not return after context cancel")
	}
}

func waitForProcessExit(t *testing.T, server *AgentServer, tag string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := server.List(context.Background(), &pb.ListProcessesRequest{})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		found := false
		for _, process := range list.Processes {
			if process.Tag == tag {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %q was not removed before deadline", tag)
}

type blockingAfterStartStream struct {
	ctx     context.Context
	startCh chan struct{}
	once    sync.Once
}

func (s *blockingAfterStartStream) Send(event *pb.ProcessEvent) error {
	if event.GetStart() != nil {
		s.once.Do(func() {
			close(s.startCh)
		})
		return nil
	}
	<-s.ctx.Done()
	return s.ctx.Err()
}

func (s *blockingAfterStartStream) Context() context.Context {
	return s.ctx
}
