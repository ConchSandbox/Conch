package clh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/internal/vmm/driver"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	defaultVmmBinary          = "/usr/local/bin/cloud-hypervisor"
	pmemDevicesPerPciSegment  = 24
	defaultCloudHypervisorPci = 1
)

const startScriptCLH = `ip netns exec {{ .NamespaceID }} \
{{ .VmmBinaryPath }} \
{{ if .PlatformArgs }}{{ .PlatformArgs }} \
{{ end }}\
--cpus boot={{ .CPUBoot }},max={{ .CPUMax }},max_phys_bits=42 \
--kernel {{ .KernelPath }} \
--initramfs {{ .InitrdPath }} \
{{ .PmemArgs }} \
--memory "size=0" \
--memory-zone "id=mem0,size={{ .MemorySize }},file={{ .MemoryPath }},shared=on" \
--cmdline "console=hvc0 root=/dev/ram0 rw debug conch.sandbox_id={{ .SandboxId }}" \
--api-socket fd={{ .ApiSocketFd }} \
--console null \
--net "tap={{ .TapName }}" \
--vsock "cid={{ .VsockCID }},socket={{ .VsockSocketPath }}" \
--event-monitor fd={{ .EventMonitorFd }} \
--seccomp false`

// -vv use for printing log when test
// Current VM lifecycle is bound to Conch; Conch exit causes VM process termination. Detachment needed for follow-up.

const resumeScriptCLH = `ip netns exec {{ .NamespaceID }} \
{{ .VmmBinaryPath }} \
--api-socket fd={{ .ApiSocketFd }} \
--event-monitor fd={{ .EventMonitorFd }} \
--seccomp false`

type StartScriptCLHArgs struct {
	VmmBinaryPath   string
	CPUBoot         int64
	CPUMax          int64
	MemorySize      string
	MemoryPath      string
	KernelPath      string
	InitrdPath      string
	PlatformArgs    string
	PmemArgs        string
	NamespaceID     string
	TapName         string
	VsockCID        uint32
	VsockSocketPath string
	SandboxId       string
	EventMonitorFd  int
	ApiSocketFd     int
}

type CLHClient struct {
	vmmType    int
	socketPath string
	fds        *VmmFds
}

func NewCLHClient(vmmType int, socketPath string) *CLHClient {
	return &CLHClient{
		vmmType:    vmmType,
		socketPath: socketPath,
	}
}

// Cloud-hypervisor event types.
const (
	EventBooted = "booted"
)

// VmmFds holds file descriptors for communicating with cloud-hypervisor.
type VmmFds struct {
	mu           sync.Mutex
	conchEventFd int
	clhEventFd   int
	apiSocketFd  int
	socketPath   string // for cleanup
}

func closeFd(fd *int) {
	if *fd > 0 {
		_ = unix.Close(*fd)
		*fd = 0
	}
}

// closeChildFdsInParent closes the parent's copies of the descriptors inherited
// by cloud-hypervisor. The child process keeps its copies after Start.
func (f *VmmFds) closeChildFdsInParent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	closeFd(&f.clhEventFd)
	closeFd(&f.apiSocketFd)
}

// cleanup closes all fds and removes the socket file.
func (f *VmmFds) cleanup() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	closeFd(&f.conchEventFd)
	closeFd(&f.clhEventFd)
	closeFd(&f.apiSocketFd)
	if f.socketPath != "" {
		_ = unix.Unlink(f.socketPath)
		f.socketPath = ""
	}
}

// createVmmFds creates file descriptors needed for cloud-hypervisor communication:
// - event-monitor socketpair
// - api-socket unix socket (bind + listen)
func createVmmFds(vmmSocketPath string) (*VmmFds, error) {
	vmmFds := &VmmFds{socketPath: vmmSocketPath}

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		vmmFds.cleanup()
		return nil, fmt.Errorf("failed to create socketpair: %w", err)
	}
	vmmFds.conchEventFd = fds[0]
	vmmFds.clhEventFd = fds[1]

	// Set conchd fd to close-on-exec (cloud-hypervisor fd should NOT be close-on-exec).
	unix.CloseOnExec(vmmFds.conchEventFd)

	vmmFds.apiSocketFd, err = unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		vmmFds.cleanup()
		return nil, fmt.Errorf("failed to create api socket: %w", err)
	}

	sockAddr := &unix.SockaddrUnix{Name: vmmSocketPath}
	if err := unix.Bind(vmmFds.apiSocketFd, sockAddr); err != nil {
		vmmFds.cleanup()
		return nil, fmt.Errorf("failed to bind api socket: %w", err)
	}
	if err := unix.Listen(vmmFds.apiSocketFd, 1); err != nil {
		vmmFds.cleanup()
		return nil, fmt.Errorf("failed to listen on api socket: %w", err)
	}

	return vmmFds, nil
}

func (c *CLHClient) PrepareLaunch(args *driver.ResourceArgs) error {
	fds, err := createVmmFds(c.socketPath)
	if err != nil {
		return err
	}
	c.fds = fds
	args.EventMonitorFd = fds.clhEventFd
	args.ApiSocketFd = fds.apiSocketFd
	return nil
}

func (c *CLHClient) AfterProcessStart() {
	if c.fds != nil {
		c.fds.closeChildFdsInParent()
	}
}

func (c *CLHClient) Cleanup() {
	if c.fds != nil {
		c.fds.cleanup()
	}
}

func (c *CLHClient) WaitForCreateReady(ctx context.Context, processExited <-chan error) error {
	return c.waitForSourceEvent(ctx, "vm", EventBooted)
}

func (c *CLHClient) WaitForResumeReady(ctx context.Context, processExited <-chan error) error {
	return nil
}

// waitForSourceEvent waits for a specific event from a specific source.
func (c *CLHClient) waitForSourceEvent(ctx context.Context, source, eventName string) error {
	if c.fds == nil || c.fds.conchEventFd <= 0 {
		return nil
	}

	logger := ulog.GetLogger()
	logger.Info("Waiting for VM event", ulog.F("event_fd", c.fds.conchEventFd), ulog.F("source", source), ulog.F("event", eventName))
	if err := waitVmReadyFd(ctx, c.fds.conchEventFd, source, eventName); err != nil {
		return fmt.Errorf("error waiting for %s/%s event: %w", source, eventName, err)
	}

	return nil
}

// CloudHypervisorEvent represents an event from cloud-hypervisor event-monitor.
type CloudHypervisorEvent struct {
	Timestamp interface{} `json:"timestamp"`
	Source    string      `json:"source"`
	Event     string      `json:"event"`
}

type cloudHypervisorEventParser struct {
	pending []byte
}

func (p *cloudHypervisorEventParser) parse(chunk []byte) ([]CloudHypervisorEvent, error) {
	p.pending = append(p.pending, chunk...)

	var parsed []CloudHypervisorEvent
	decoder := json.NewDecoder(bytes.NewReader(p.pending))
	for {
		var event CloudHypervisorEvent
		err := decoder.Decode(&event)
		if err == nil {
			parsed = append(parsed, event)
			continue
		}

		consumed := int(decoder.InputOffset())
		switch {
		case errors.Is(err, io.EOF):
			p.pending = bytes.TrimLeft(p.pending[consumed:], " \t\r\n")
			return parsed, nil
		case errors.Is(err, io.ErrUnexpectedEOF):
			p.pending = bytes.TrimLeft(p.pending[consumed:], " \t\r\n")
			return parsed, nil
		default:
			return parsed, fmt.Errorf("decode event monitor payload: %w", err)
		}
	}
}

func (p *cloudHypervisorEventParser) readFromFd(eventFd int, buf []byte) ([]CloudHypervisorEvent, error) {
	readN, readErr := unix.Read(eventFd, buf)
	if readN <= 0 {
		if readErr == unix.EAGAIN || readErr == unix.EWOULDBLOCK {
			return nil, nil
		}
		if readErr != nil {
			return nil, fmt.Errorf("read error: %w", readErr)
		}
		return nil, io.EOF
	}
	return p.parse(buf[:readN])
}

// parseEventsFromFd reads and parses JSON events from a file descriptor.
// It returns all parsed events or an error if parsing fails.
func parseEventsFromFd(eventFd int, buf []byte) ([]CloudHypervisorEvent, error) {
	var parser cloudHypervisorEventParser
	return parser.readFromFd(eventFd, buf)
}

// waitVmReadyFd waits for the VM to be ready by reading events from fd.
// It watches for the specified event which indicates the VM has started successfully.
func waitVmReadyFd(ctx context.Context, eventFd int, waitForSource, waitForEvent string) error {
	epollFd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("failed to create epoll: %w", err)
	}
	defer unix.Close(epollFd)

	epollEvent := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(eventFd)}
	if err := unix.EpollCtl(epollFd, unix.EPOLL_CTL_ADD, eventFd, &epollEvent); err != nil {
		return fmt.Errorf("failed to add event fd to epoll: %w", err)
	}

	events := make([]unix.EpollEvent, 1)
	buf := make([]byte, 4096)
	var parser cloudHypervisorEventParser

	for {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled waiting for VM ready: %w", ctx.Err())
		}

		timeoutMs := -1 // -1 means wait indefinitely
		if deadline, ok := ctx.Deadline(); ok {
			timeoutMs = int(time.Until(deadline).Milliseconds())
			if timeoutMs <= 0 {
				return fmt.Errorf("timeout waiting for VM ready event")
			}
		}

		n, err := unix.EpollWait(epollFd, events, timeoutMs)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("epoll wait error: %w", err)
		}

		if n == 0 {
			return fmt.Errorf("timeout waiting for VM ready event")
		}

		clhEvents, err := parser.readFromFd(eventFd, buf)
		if err != nil {
			return err
		}
		for _, event := range clhEvents {
			if event.Source == waitForSource && event.Event == waitForEvent {
				return nil
			}
		}
	}
}

func isServerError(statusCode int) bool {
	switch statusCode {
	case http.StatusOK, http.StatusContinue, http.StatusNoContent:
		return false
	default:
		return true
	}
}

func buildRequest(method, fullCommand, requestBody string) string {
	request := fmt.Sprintf("%s /api/v1/vm.%s HTTP/1.1\r\n", method, fullCommand)
	request += "Host: localhost\r\n"
	request += "Accept: */*\r\n"

	if len(requestBody) != 0 {
		request += fmt.Sprintf("Content-Length: %d\r\n", len(requestBody))
	}

	request += "\r\n"

	if len(requestBody) != 0 {
		request += requestBody
	}

	return request
}

func (clh *CLHClient) BuildStartCmd(args *driver.ResourceArgs, isResume bool) (string, error) {
	logger := ulog.GetLogger()

	vmmBinaryPath := defaultVmmBinary
	if path, err := exec.LookPath("cloud-hypervisor"); err == nil {
		vmmBinaryPath = path
	}

	clhArgs := StartScriptCLHArgs{
		VmmBinaryPath:   vmmBinaryPath,
		CPUBoot:         args.CPUBoot,
		CPUMax:          args.CPUMax,
		MemorySize:      strconv.FormatInt(args.MemorySize, 10) + "M",
		MemoryPath:      args.MemoryPath,
		KernelPath:      args.KernelPath,
		InitrdPath:      args.InitrdPath,
		PlatformArgs:    buildPlatformArgs(args.PmemPaths),
		PmemArgs:        buildPmemArgs(args.PmemPaths),
		NamespaceID:     args.NamespaceID,
		TapName:         args.TapName,
		VsockCID:        args.VsockCID,
		VsockSocketPath: args.VsockSocketPath,
		SandboxId:       args.SandboxId,
		EventMonitorFd:  args.EventMonitorFd,
		ApiSocketFd:     args.ApiSocketFd,
	}

	_, err := os.Stat(clhArgs.VmmBinaryPath)
	if err != nil {
		logger.Error("Error stating VMM binary",
			ulog.F("path", clhArgs.VmmBinaryPath),
			ulog.F("error", err),
		)
		return "", fmt.Errorf("error stating vmm binary: %w", err)
	}

	var scriptContent string
	if isResume {
		scriptContent = resumeScriptCLH
	} else {
		scriptContent = startScriptCLH
	}

	templateCLH := template.Must(template.New("clh-start").Parse(scriptContent))

	var scriptBuffer bytes.Buffer
	err = templateCLH.Execute(&scriptBuffer, clhArgs)
	if err != nil {
		logger.Error("Error executing CLH start script template",
			ulog.F("error", err),
		)
		return "", fmt.Errorf("error executing CLH start script template: %w", err)
	}

	// debug
	script := scriptBuffer.String()
	logger.Debug("Build start command", ulog.F("script", script))
	return script, nil
}

func buildPlatformArgs(pmemPaths []string) string {
	pmemCount := nonEmptyPathCount(pmemPaths)
	segments := pciSegmentsForPmemDeviceCount(pmemCount)
	if segments <= defaultCloudHypervisorPci {
		return ""
	}
	return fmt.Sprintf(`--platform "num_pci_segments=%d"`, segments)
}

func pciSegmentsForPmemDeviceCount(pmemCount int) int {
	if pmemCount <= 0 {
		return defaultCloudHypervisorPci
	}
	return (pmemCount + pmemDevicesPerPciSegment - 1) / pmemDevicesPerPciSegment
}

func nonEmptyPathCount(paths []string) int {
	count := 0
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			count++
		}
	}
	return count
}

func buildPmemArgs(paths []string) string {
	args := make([]string, 0, len(paths)+1)
	args = append(args, "--pmem")
	pmemIndex := 0
	segments := pciSegmentsForPmemDeviceCount(nonEmptyPathCount(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		pciSegment := pmemIndex / pmemDevicesPerPciSegment
		pmemIndex++
		pmemArg := fmt.Sprintf("file=%s,discard_writes=on", path)
		if segments > defaultCloudHypervisorPci {
			pmemArg += fmt.Sprintf(",pci_segment=%d", pciSegment)
		}
		args = append(args, pmemArg)
	}
	return strings.Join(args, " \\\n")
}

func (c *CLHClient) requestApi(method, fullCommand, requestBody string) error {
	logger := ulog.GetLogger()

	request := buildRequest(method, fullCommand, requestBody)
	logger.Debug("Sending API request", ulog.F("request", request))

	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		logger.Error("Failed to connect to socket",
			ulog.F("socket", c.socketPath),
			ulog.F("error", err),
		)
		return fmt.Errorf("failed to connect to socket: %w", err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte(request))
	if err != nil {
		logger.Error("Failed to send request",
			ulog.F("error", err),
		)
		return fmt.Errorf("failed to send request: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		logger.Error("Failed to parse HTTP response",
			ulog.F("error", err),
		)
		return fmt.Errorf("failed to parse HTTP response: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("Failed to read response body",
			ulog.F("error", err),
		)
		return fmt.Errorf("failed to read response body: %w", err)
	}

	if isServerError(resp.StatusCode) {
		logger.Error("Server returned error",
			ulog.F("status", resp.Status),
			ulog.F("body", string(body)),
		)
		return fmt.Errorf("server returned error: %s, body: %s", resp.Status, string(body))
	}

	logger.Debug("API response", ulog.F("body", string(body)))
	return nil
}

func (c *CLHClient) CheckDaemonAlive() error {
	// TODO: call conchd GetHealth
	return nil
}

func (c *CLHClient) PauseVM() error {
	logger := ulog.GetLogger()
	logger.Debug("Pausing VM")
	return c.requestApi("PUT", "pause", "")
}

func (c *CLHClient) ResumeVM() error {
	logger := ulog.GetLogger()
	logger.Debug("Resuming VM")
	return c.requestApi("PUT", "resume", "")
}

func (c *CLHClient) DeleteVM() error {
	logger := ulog.GetLogger()
	logger.Debug("Deleting VM")
	return c.requestApi("PUT", "delete", "")
}

func (c *CLHClient) CreateSnapshot(snapfilePath string) error {
	logger := ulog.GetLogger()
	logger.Info("Creating snapshot",
		ulog.F("path", snapfilePath),
	)
	if err := os.MkdirAll(snapfilePath, 0o750); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}

	requestBody := struct {
		DestinationURL string `json:"destination_url"`
	}{
		DestinationURL: "file://" + snapfilePath,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		logger.Error("Failed to marshal JSON",
			ulog.F("error", err),
		)
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return c.requestApi("PUT", "snapshot", string(jsonBody))
}

func (c *CLHClient) LoadSnapshot(snapfilePath string, preferVNC bool) error {
	logger := ulog.GetLogger()
	logger.Info("Loading snapshot",
		ulog.F("path", snapfilePath),
		ulog.F("preferVNC", preferVNC),
	)

	requestBody := struct {
		SourceURL string `json:"source_url"`
		PreferVNC bool   `json:"preferVNC"`
	}{
		SourceURL: "file://" + snapfilePath,
		PreferVNC: preferVNC,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		logger.Error("Failed to marshal JSON",
			ulog.F("error", err),
		)
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return c.requestApi("PUT", "restore", string(jsonBody))
}
