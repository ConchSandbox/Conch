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
	"path/filepath"
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

const startScriptStratovirt = `ip netns exec {{ .NamespaceID }} \
{{ .VmmBinaryPath }} \
-machine {{ .MachineType }} \
-kernel {{ .KernelPath }} \
-initrd {{ .RootfsPath }} \
-append "console={{ .ConsoleDevice }} reboot=k quiet panic=1 root=/dev/ram0 rw conch.sandbox_id={{ .SandboxId }}" \
-m {{ .MemorySize }}M \
-smp {{ .CPUBoot }} \
-qmp unix:{{ .VmmSocket }},server,nowait \
-serial socket,path={{ .SerialSocket }},server,nowait \
-netdev tap,id=net0,ifname={{ .TapName }} \
-device virtio-net-pci,netdev=net0,id=net0,bus=pcie.0,addr=0x10 \
{{ .PmemDevices }} \
-device vhost-vsock-pci,id=vsock0,guest-cid={{ .VsockCID }},bus=pcie.0,addr=0x11 \
-disable-seccomp`

const resumeScriptStratovirt = `ip netns exec {{ .NamespaceID }} \
{{ .VmmBinaryPath }} \
-machine {{ .MachineType }} \
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
-incoming file:{{ .SnapfilePath }}`

type StartScriptStratovirtArgs struct {
	VmmBinaryPath string
	CPUBoot       int64
	CPUMax        int64
	MemorySize    string
	MachineType   string
	ConsoleDevice string
	MemoryPath    string
	KernelPath    string
	RootfsPath    string
	NamespaceID   string
	TapName       string
	VmmSocket     string
	SerialSocket  string
	SnapfilePath  string
	SandboxId     string
	VsockCID      uint32
	PmemDevices   string
}

type StratovirtClient struct {
	vmmType    int
	socketPath string
	config     *stratovirtSnapshotConfig
}

type stratovirtSnapshotConfig struct {
	memorySize int64
	kernelPath string
	initrdPath string
	vsockCID   uint32
}

func NewStratovirtClient(vmmType int, socketPath string) *StratovirtClient {
	return &StratovirtClient{
		vmmType:    vmmType,
		socketPath: socketPath,
	}
}

func (s *StratovirtClient) PrepareLaunch(args *driver.ResourceArgs) error {
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
		VmmBinaryPath: vmmBinaryPath,
		CPUBoot:       args.CPUBoot,
		CPUMax:        args.CPUMax,
		MemorySize:    strconv.FormatInt(args.MemorySize, 10),
		MachineType:   machineType,
		ConsoleDevice: consoleDevice,
		MemoryPath:    args.MemoryPath,
		KernelPath:    args.KernelPath,
		RootfsPath:    args.InitrdPath,
		NamespaceID:   args.NamespaceID,
		TapName:       args.TapName,
		VmmSocket:     s.socketPath,
		SerialSocket:  s.socketPath + ".serial",
		SnapfilePath:  args.SnapfilePath,
		SandboxId:     args.SandboxId,
		VsockCID:      args.VsockCID,
		PmemDevices:   buildStratovirtPmemDevices(args.PmemPaths),
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

	s.config = &stratovirtSnapshotConfig{
		memorySize: args.MemorySize,
		kernelPath: args.KernelPath,
		initrdPath: args.InitrdPath,
		vsockCID:   args.VsockCID,
	}

	script := scriptBuffer.String()
	logger.Debug("Build start command (Stratovirt)", ulog.F("script", script))
	return script, nil
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

func (s *StratovirtClient) CheckDaemonAlive() error {
	logger := ulog.GetLogger()

	for i := 0; i < 60; i++ {
		response, err := s.executeQMPCommandWithResponse("query-status", nil)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		returnVal, ok := response["return"]
		if !ok {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		returnMap, ok := returnVal.(map[string]any)
		if !ok {
			time.Sleep(100 * time.Millisecond)
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
				time.Sleep(100 * time.Millisecond)
				continue
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if status == "running" || running {
			logger.Info("VM is running")
			return nil
		}

		time.Sleep(100 * time.Millisecond)
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

// StratoVirt resume is driven by "-incoming file:<path>" at process launch.
// CheckDaemonAlive sends "cont" if the restored VM is paused.
func (s *StratovirtClient) ResumeVM() error {
	return nil
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
			if err := s.generateSnapshotConfig(snapfilePath); err != nil {
				return fmt.Errorf("failed to generate snapshot config: %w", err)
			}
			return nil
		case "failed":
			return fmt.Errorf("snapshot failed")
		}

		time.Sleep(stratovirtSnapshotPollInterval)
	}

	return fmt.Errorf("snapshot timeout after %v", stratovirtSnapshotPollTimeout)
}

func (s *StratovirtClient) generateSnapshotConfig(snapfilePath string) error {
	logger := ulog.GetLogger()

	if s.config == nil {
		return fmt.Errorf("snapshot config not available")
	}

	config := map[string]any{
		"payload": map[string]any{
			"kernel":    s.config.kernelPath,
			"initramfs": s.config.initrdPath,
		},
		"memory": map[string]any{
			"size": s.config.memorySize * 1024 * 1024,
			"zones": []map[string]any{
				{
					"id":     "mem0",
					"size":   fmt.Sprintf("%dM", s.config.memorySize),
					"shared": true,
				},
			},
		},
		"vsock": map[string]any{
			"cid": s.config.vsockCID,
		},
	}

	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot config: %w", err)
	}

	configPath := filepath.Join(snapfilePath, "config.json")
	if err := os.WriteFile(configPath, configData, 0640); err != nil {
		return fmt.Errorf("failed to write snapshot config: %w", err)
	}

	logger.Info("Generated snapshot config file", ulog.F("path", configPath))
	return nil
}

// StratoVirt consumes the snapshot during process launch via "-incoming file:<path>".
// There is no separate QMP load-snapshot step here.
func (s *StratovirtClient) LoadSnapshot(snapfilePath string, preferVNC bool) error {
	return nil
}
