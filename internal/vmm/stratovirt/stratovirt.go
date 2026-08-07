package stratovirt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/openeuler/Conch/internal/vmm/driver"
	"github.com/openeuler/Conch/pkg/ulog"
)

const defaultStratovirtBinary = "/usr/bin/stratovirt"

func stratovirtMachineType() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "x86_64":
		return "q35", nil
	case "arm64", "aarch64":
		return "virt", nil
	default:
		return "", fmt.Errorf("unsupported arch for stratovirt machine type: %s", runtime.GOARCH)
	}
}

func stratovirtConsoleDevice() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "x86_64":
		return "ttyS0", nil
	case "arm64", "aarch64":
		return "ttyAMA0", nil
	default:
		return "", fmt.Errorf("unsupported arch for stratovirt console device: %s", runtime.GOARCH)
	}
}

const startScriptStratovirt = `{{ .NSenterPath }} --net={{ .NetNSPath }} -- \
{{ .VmmBinaryPath }} \
-machine {{ .MachineType }}{{ .MachineOpts }} \
-kernel {{ .KernelPath }} \
-initrd {{ .RootfsPath }} \
-append "console={{ .ConsoleDevice }} reboot=k quiet panic=1 root=/dev/ram0 rw conch.sandbox_id={{ .SandboxId }}{{ .SharefsCmdline }}" \
-m {{ .MemorySize }}M \
-smp {{ .CPUBoot }} \
-qmp unix:{{ .VmmSocket }},server,nowait \
-serial socket,path={{ .SerialSocket }},server,nowait \
-netdev tap,id=net0,ifname={{ .TapName }} \
-device virtio-net-pci,netdev=net0,id=net0,bus=pcie.0,addr=0x10 \
{{ .VirtioFSDevices }} \
{{ .PmemDevices }} \
-device vhost-vsock-pci,id=vsock0,guest-cid={{ .VsockCID }},bus=pcie.0,addr=0x11 \
-disable-seccomp`

const resumeScriptStratovirt = `{{ .NSenterPath }} --net={{ .NetNSPath }} -- \
{{ .VmmBinaryPath }} \
-machine {{ .MachineType }}{{ .MachineOpts }} \
-kernel {{ .KernelPath }} \
-initrd {{ .RootfsPath }} \
-append "console={{ .ConsoleDevice }} reboot=k quiet panic=1 root=/dev/ram0 rw" \
-m {{ .MemorySize }}M \
-smp {{ .CPUBoot }} \
-qmp unix:{{ .VmmSocket }},server,nowait \
-serial socket,path={{ .SerialSocket }},server,nowait \
-netdev tap,id=net0,ifname={{ .TapName }} \
-device virtio-net-pci,netdev=net0,id=net0,bus=pcie.0,addr=0x10 \
{{ .PmemDevices }} \
-device vhost-vsock-pci,id=vsock0,guest-cid={{ .VsockCID }},bus=pcie.0,addr=0x11 \
-disable-seccomp \
-incoming file:{{ .SnapfilePath }},mapped=true`

type StartScriptStratovirtArgs struct {
	NSenterPath     string
	VmmBinaryPath   string
	CPUBoot         int64
	CPUMax          int64
	MemorySize      string
	MachineType     string
	ConsoleDevice   string
	KernelPath      string
	RootfsPath      string
	NetNSPath       string
	TapName         string
	VmmSocket       string
	SerialSocket    string
	SnapfilePath    string
	SandboxId       string
	VsockCID        uint32
	PmemDevices     string
	VirtioFSDevices string
	SharefsCmdline  string
	// MachineOpts is appended to -machine (e.g. ",mem-share=on" when virtiofs
	// mounts are present, since vhost-user-fs needs guest memory shared with
	// the userspace virtiofsd backend).
	MachineOpts string
}

type StratovirtClient struct {
	vmmType    int
	socketPath string
}

func NewStratovirtClient(vmmType int, socketPath string) *StratovirtClient {
	return &StratovirtClient{
		vmmType:    vmmType,
		socketPath: socketPath,
	}
}

func (s *StratovirtClient) PrepareLaunch(args *driver.ResourceArgs, isResume bool) error {
	return nil
}

func (s *StratovirtClient) AfterProcessStart() {}

func (s *StratovirtClient) Cleanup() {}

func (s *StratovirtClient) WaitForCreateReady(ctx context.Context, processExited <-chan error) error {
	return waitForVmmSocket(ctx, s.socketPath, processExited)
}

func (s *StratovirtClient) WaitForResumeReady(ctx context.Context, processExited <-chan error) error {
	return waitForVmmSocket(ctx, s.socketPath, processExited)
}

func waitForVmmSocket(ctx context.Context, socketPath string, processExited <-chan error) error {
	logger := ulog.GetLogger()

	delay := 2 * time.Millisecond
	const maxDelay = 100 * time.Millisecond
	for {
		if _, err := os.Stat(socketPath); err == nil {
			logger.Debug("VMM socket ready", ulog.F("socket", socketPath))
			return nil
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("cancelled waiting for vmm socket %s: %w", socketPath, ctx.Err())
		case waitErr, ok := <-processExited:
			timer.Stop()
			if !ok || waitErr == nil {
				return fmt.Errorf("vmm process exited before vmm socket %s was ready", socketPath)
			}
			return fmt.Errorf("vmm process exited before vmm socket %s was ready: %w", socketPath, waitErr)
		case <-timer.C:
		}

		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (s *StratovirtClient) BuildStartCmd(args *driver.ResourceArgs, isResume bool) (string, error) {
	logger := ulog.GetLogger()
	nsenterPath, err := exec.LookPath("nsenter")
	if err != nil {
		return "", fmt.Errorf("resolve nsenter binary: %w", err)
	}

	vmmBinaryPath := defaultStratovirtBinary
	if path, err := exec.LookPath("stratovirt"); err == nil {
		vmmBinaryPath = path
	}

	machineType, err := stratovirtMachineType()
	if err != nil {
		logger.Error("Failed to resolve Stratovirt machine type", ulog.F("error", err))
		return "", err
	}
	consoleDevice, err := stratovirtConsoleDevice()
	if err != nil {
		logger.Error("Failed to resolve Stratovirt console device", ulog.F("error", err))
		return "", err
	}

	stArgs := StartScriptStratovirtArgs{
		NSenterPath:     nsenterPath,
		VmmBinaryPath:   vmmBinaryPath,
		CPUBoot:         args.CPUBoot,
		CPUMax:          args.CPUMax,
		MemorySize:      strconv.FormatInt(args.MemorySize, 10),
		MachineType:     machineType,
		ConsoleDevice:   consoleDevice,
		KernelPath:      args.KernelPath,
		RootfsPath:      args.InitrdPath,
		NetNSPath:       args.NetNSPath,
		TapName:         args.TapName,
		VmmSocket:       s.socketPath,
		SerialSocket:    s.socketPath + ".serial",
		SnapfilePath:    args.SnapfilePath,
		SandboxId:       args.SandboxId,
		VsockCID:        args.VsockCID,
		PmemDevices:     buildStratovirtPmemDevices(args.PmemPaths),
		VirtioFSDevices: buildStratovirtVirtioFSDevices(args.VirtioFS, len(args.PmemPaths)),
		SharefsCmdline:  buildSharefsCmdline(args.VirtioFS),
		MachineOpts:     stratovirtMachineOpts(args.VirtioFS),
	}

	if _, err = os.Stat(stArgs.VmmBinaryPath); err != nil {
		logger.Error("Error stating Stratovirt binary",
			ulog.F("path", stArgs.VmmBinaryPath),
			ulog.F("error", err),
		)
		return "", fmt.Errorf("error stating stratovirt binary: %w", err)
	}

	scriptContent := startScriptStratovirt
	if isResume {
		scriptContent = resumeScriptStratovirt
	}
	templateSt := template.Must(template.New("stratovirt-start").Parse(scriptContent))

	var scriptBuffer bytes.Buffer
	if err = templateSt.Execute(&scriptBuffer, stArgs); err != nil {
		logger.Error("Error executing Stratovirt start script template",
			ulog.F("error", err),
		)
		return "", fmt.Errorf("error executing stratovirt start script template: %w", err)
	}

	script := scriptBuffer.String()
	logger.Debug("Build start command (Stratovirt)", ulog.F("script", script))
	return script, nil
}

// buildStratovirtVirtioFSDevices renders the -chardev/-device pair for each
// virtiofs mount. Two StratoVirt requirements drove this shape:
//   - vhost-user-fs-pci MUST carry an explicit bus=pcie.0,addr=...; StratoVirt
//     rejects the device with "Should set bus"/"Should set addr" otherwise.
//   - The address must not collide with virtio-pmem devices, which occupy the
//     0x12.. range (one per rootfs layer). fs devices are therefore placed
//     right above the pmem range.
func buildStratovirtVirtioFSDevices(devices []driver.VirtioFSDevice, pmemCount int) string {
	var args []string
	baseAddr := 0x12 + pmemCount
	for i, device := range devices {
		if strings.TrimSpace(device.Socket) == "" || strings.TrimSpace(device.Tag) == "" {
			continue
		}
		charID := fmt.Sprintf("charfs%d", i)
		devID := fmt.Sprintf("fs%d", i)
		addr := fmt.Sprintf("0x%x", baseAddr+i)
		args = append(args,
			fmt.Sprintf("-chardev socket,id=%s,path=%s", charID, device.Socket),
			fmt.Sprintf("-device vhost-user-fs-pci,id=%s,chardev=%s,tag=%s,bus=pcie.0,addr=%s", devID, charID, device.Tag, addr),
		)
	}
	return strings.Join(args, " \\\n")
}

// stratovirtMachineOpts returns machine-level options appended to -machine.
// vhost-user-fs requires guest memory to be shared with the virtiofsd backend
// process, so mem-share=on must be set whenever a virtiofs mount is attached.
func stratovirtMachineOpts(virtioFS []driver.VirtioFSDevice) string {
	if len(virtioFS) == 0 {
		return ""
	}
	return ",mem-share=on"
}

// buildSharefsCmdline appends the minimal sharefs switch to the kernel cmdline
// when a virtiofs device is attached. The per-mount volume table is delivered
// through the shared dir's config.json (read by the guest agent), NOT via the
// cmdline, so the cmdline payload is constant-size and never collides with the
// Stratovirt kernel params <=255 byte limit.
func buildSharefsCmdline(virtioFS []driver.VirtioFSDevice) string {
	if len(virtioFS) == 0 {
		return ""
	}
	return " conch.sharefs=virtiofs"
}

func buildStratovirtPmemDevices(pmemPaths []string) string {
	if len(pmemPaths) == 0 {
		return ""
	}

	var devices []string
	for i, path := range pmemPaths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}

		size := stratovirtFileSize(path)
		if size == 0 {
			continue
		}

		memID := fmt.Sprintf("pmem%d", i)
		devID := fmt.Sprintf("pmem%dpci", i)
		addr := fmt.Sprintf("0x%x", 0x12+i)
		sizeStr := fmt.Sprintf("%dM", size/(1024*1024))

		object := fmt.Sprintf("-object memory-backend-file,size=%s,id=%s,mem-path=%s,share=off", sizeStr, memID, path)
		device := fmt.Sprintf("-device virtio-pmem-pci,id=%s,memdev=%s,bus=pcie.0,addr=%s", devID, memID, addr)
		devices = append(devices, object, device)
	}

	return strings.Join(devices, " \\\n")
}

func stratovirtFileSize(path string) int64 {
	logger := ulog.GetLogger()
	info, err := os.Stat(path)
	if err != nil {
		logger.Warn("Failed to get file size", ulog.F("path", path), ulog.F("error", err))
		return 0
	}
	size := info.Size()
	logger.Debug("Got file size", ulog.F("path", path), ulog.F("size", size))
	return size
}

func (s *StratovirtClient) connectQMP() (net.Conn, *bufio.Reader, error) {
	logger := ulog.GetLogger()

	conn, err := net.Dial("unix", s.socketPath)
	if err != nil {
		logger.Error("Failed to connect to QMP socket",
			ulog.F("socket", s.socketPath),
			ulog.F("error", err),
		)
		return nil, nil, fmt.Errorf("failed to connect to qmp socket: %w", err)
	}

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("failed to read qmp greeting: %w", err)
	}
	logger.Debug("QMP greeting received", ulog.F("greeting", strings.TrimSpace(greeting)))

	qmpCapabilities := `{"execute":"qmp_capabilities"}` + "\n"
	if _, err = conn.Write([]byte(qmpCapabilities)); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("failed to send qmp_capabilities: %w", err)
	}

	resp, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("failed to read qmp_capabilities response: %w", err)
	}
	logger.Debug("QMP capabilities response", ulog.F("response", strings.TrimSpace(resp)))

	return conn, reader, nil
}

func (s *StratovirtClient) executeQMPCommand(command string, arguments map[string]any) error {
	_, err := s.executeQMPCommandWithResponse(command, arguments)
	return err
}

func (s *StratovirtClient) executeQMPCommandWithResponse(command string, arguments map[string]any) (map[string]any, error) {
	logger := ulog.GetLogger()

	conn, reader, err := s.connectQMP()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	cmd := map[string]any{
		"execute": command,
	}
	if arguments != nil {
		cmd["arguments"] = arguments
	}

	jsonCmd, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal qmp command: %w", err)
	}
	jsonCmd = append(jsonCmd, '\n')
	logger.Debug("Sending QMP command", ulog.F("command", string(jsonCmd)))

	if _, err = conn.Write(jsonCmd); err != nil {
		return nil, fmt.Errorf("failed to send qmp command: %w", err)
	}

	resp, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read qmp response: %w", err)
	}
	logger.Debug("QMP response", ulog.F("response", strings.TrimSpace(resp)))

	var response map[string]any
	if err := json.Unmarshal([]byte(resp), &response); err != nil {
		return nil, fmt.Errorf("failed to parse qmp response: %w", err)
	}
	if errObj, ok := response["error"]; ok {
		return nil, fmt.Errorf("qmp command failed: %v", errObj)
	}
	return response, nil
}

func waitForAgentRetry(ctx context.Context, processExited <-chan error, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled waiting for conch-init ready: %w", ctx.Err())
		case waitErr, ok := <-processExited:
			if !ok || waitErr == nil {
				return fmt.Errorf("vmm process exited before conch-init became ready")
			}
			return fmt.Errorf("vmm process exited before conch-init became ready: %w", waitErr)
		default:
			return nil
		}
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("cancelled waiting for conch-init ready: %w", ctx.Err())
	case waitErr, ok := <-processExited:
		if !ok || waitErr == nil {
			return fmt.Errorf("vmm process exited before conch-init became ready")
		}
		return fmt.Errorf("vmm process exited before conch-init became ready: %w", waitErr)
	case <-timer.C:
		return nil
	}
}

func (s *StratovirtClient) CheckAgentAlive(ctx context.Context, processExited <-chan error) error {
	logger := ulog.GetLogger()

	for i := 0; i < 60; i++ {
		if err := waitForAgentRetry(ctx, processExited, 0); err != nil {
			return err
		}

		response, err := s.executeQMPCommandWithResponse("query-status", nil)
		if err != nil {
			if err := waitForAgentRetry(ctx, processExited, 100*time.Millisecond); err != nil {
				return err
			}
			continue
		}

		returnVal, ok := response["return"]
		if !ok {
			if err := waitForAgentRetry(ctx, processExited, 100*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		returnMap, ok := returnVal.(map[string]any)
		if !ok {
			if err := waitForAgentRetry(ctx, processExited, 100*time.Millisecond); err != nil {
				return err
			}
			continue
		}

		status, _ := returnMap["status"].(string)
		running, _ := returnMap["running"].(bool)
		logger.Debug("VM status check",
			ulog.F("status", status),
			ulog.F("running", running))

		if status == "paused" {
			logger.Info("VM is paused, sending cont command")
			if err := s.executeQMPCommand("cont", nil); err != nil {
				logger.Warn("Failed to send cont command", ulog.F("error", err))
				if err := waitForAgentRetry(ctx, processExited, 100*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			if err := waitForAgentRetry(ctx, processExited, 200*time.Millisecond); err != nil {
				return err
			}
			continue
		}

		if status == "running" || running {
			logger.Info("VM is running")
			return nil
		}

		if err := waitForAgentRetry(ctx, processExited, 100*time.Millisecond); err != nil {
			return err
		}
	}

	return fmt.Errorf("timeout waiting for VM to enter running state")
}

func (s *StratovirtClient) PauseVM() error {
	logger := ulog.GetLogger()
	logger.Debug("Pausing VM (Stratovirt)")

	status, err := s.queryStatus()
	if err != nil {
		logger.Warn("Failed to query VM status before stop", ulog.F("error", err))
	} else {
		logger.Debug("VM status before stop", ulog.F("status", status))
	}

	if err := s.executeQMPCommand("stop", nil); err != nil {
		return err
	}

	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		newStatus, _ := s.queryStatus()
		logger.Debug("VM status after stop", ulog.F("status", newStatus))
		if newStatus == "paused" || newStatus == "stopped" {
			logger.Info("VM paused successfully")
			return nil
		}
	}

	return nil
}

func (s *StratovirtClient) ResumeVM() error {
	return s.executeQMPCommand("cont", nil)
}

func (s *StratovirtClient) DeleteVM() error {
	logger := ulog.GetLogger()
	logger.Debug("Deleting VM (Stratovirt)")
	return s.executeQMPCommand("quit", nil)
}

func (s *StratovirtClient) queryStatus() (string, error) {
	response, err := s.executeQMPCommandWithResponse("query-status", nil)
	if err != nil {
		return "", err
	}

	returnVal, ok := response["return"]
	if !ok {
		return "", fmt.Errorf("invalid query-status response")
	}
	returnMap, ok := returnVal.(map[string]any)
	if !ok {
		return "", fmt.Errorf("invalid query-status response")
	}
	status, _ := returnMap["status"].(string)
	return status, nil
}

const (
	stratovirtSnapshotPollInterval = 500 * time.Millisecond
	stratovirtSnapshotPollTimeout  = 5 * time.Minute
)

func (s *StratovirtClient) CreateSnapshot(snapfilePath string) error {
	logger := ulog.GetLogger()
	logger.Info("Creating snapshot (Stratovirt)",
		ulog.F("path", snapfilePath),
	)

	status, err := s.queryStatus()
	if err != nil {
		logger.Warn("Failed to query VM status before snapshot", ulog.F("error", err))
	} else {
		logger.Debug("VM status before snapshot", ulog.F("status", status))
		if status != "paused" && status != "stopped" {
			logger.Warn("VM is not paused, attempting to pause first")
			if err := s.executeQMPCommand("stop", nil); err != nil {
				return fmt.Errorf("failed to pause vm: %w", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	args := map[string]any{
		"uri": "file:" + snapfilePath,
	}
	if err := s.executeQMPCommand("migrate", args); err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	deadline := time.Now().Add(stratovirtSnapshotPollTimeout)
	for time.Now().Before(deadline) {
		response, err := s.executeQMPCommandWithResponse("query-migrate", nil)
		if err != nil {
			time.Sleep(stratovirtSnapshotPollInterval)
			continue
		}

		returnVal, ok := response["return"]
		if !ok {
			time.Sleep(stratovirtSnapshotPollInterval)
			continue
		}
		returnMap, ok := returnVal.(map[string]any)
		if !ok {
			time.Sleep(stratovirtSnapshotPollInterval)
			continue
		}
		status, _ := returnMap["status"].(string)
		logger.Debug("Snapshot status", ulog.F("status", status))
		switch status {
		case "completed":
			logger.Info("Snapshot completed successfully")
			return nil
		case "failed":
			return fmt.Errorf("snapshot failed")
		}

		time.Sleep(stratovirtSnapshotPollInterval)
	}

	return fmt.Errorf("snapshot timeout after %v", stratovirtSnapshotPollTimeout)
}

// StratoVirt consumes the snapshot during process launch via "-incoming file:<path>".
// There is no separate QMP load-snapshot step here.
func (s *StratovirtClient) LoadSnapshot(snapfilePath string, preferVNC bool) error {
	return nil
}
