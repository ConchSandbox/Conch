package vmm

import (
	"bufio"
	"bytes"
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

	"github.com/openeuler/Conch/pkg/ulog"
)

const defaultStratovirtBinary = "/usr/bin/stratovirt"

func getMachineType() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "x86_64":
		return "q35", nil
	case "arm64", "aarch64":
		return "virt", nil
	default:
		return "", fmt.Errorf("unsupported arch for stratovirt machine type: %s", runtime.GOARCH)
	}
}

func getConsoleDevice() (string, error) {
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
	config     *snapshotConfig
}

type snapshotConfig struct {
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

func (s *StratovirtClient) BuildStartCmd(args *ResourceArgs, isResume bool) (string, error) {
	logger := ulog.GetLogger()

	vmmBinaryPath := defaultStratovirtBinary
	if path, err := exec.LookPath("stratovirt"); err == nil {
		vmmBinaryPath = path
	}

	pmemDevices := buildPmemDevices(args.PmemPaths, isResume)

	machineType, err := getMachineType()
	if err != nil {
		logger.Error("Failed to resolve machine type", ulog.F("error", err))
		return "", err
	}
	consoleDevice, err := getConsoleDevice()
	if err != nil {
		logger.Error("Failed to resolve console device", ulog.F("error", err))
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
		PmemDevices:   pmemDevices,
	}

	_, err = os.Stat(stArgs.VmmBinaryPath)
	if err != nil {
		logger.Error("Error stating Stratovirt binary",
			ulog.F("path", stArgs.VmmBinaryPath),
			ulog.F("error", err),
		)
		return "", fmt.Errorf("error stating stratovirt binary: %w", err)
	}

	var scriptContent string
	if isResume {
		scriptContent = resumeScriptStratovirt
	} else {
		scriptContent = startScriptStratovirt
	}

	templateSt := template.Must(template.New("stratovirt-start").Parse(scriptContent))

	var scriptBuffer bytes.Buffer
	err = templateSt.Execute(&scriptBuffer, stArgs)
	if err != nil {
		logger.Error("Error executing Stratovirt start script template",
			ulog.F("error", err),
		)
		return "", fmt.Errorf("error executing stratovirt start script template: %w", err)
	}

	s.config = &snapshotConfig{
		memorySize: args.MemorySize,
		kernelPath: args.KernelPath,
		initrdPath: args.InitrdPath,
		vsockCID:   args.VsockCID,
	}

	script := scriptBuffer.String()
	logger.Debug("Build start command (Stratovirt)", ulog.F("script", script))
	return script, nil
}

func buildPmemDevices(pmemPaths []string, readonly bool) string {
	if len(pmemPaths) == 0 {
		return ""
	}

	var devices []string
	for i, path := range pmemPaths {
		memId := fmt.Sprintf("pmem%d", i)
		devId := fmt.Sprintf("pmem%dpci", i)
		addr := fmt.Sprintf("0x%x", 0x12+i)

		size := getFileSize(path)
		if size == 0 {
			continue
		}

		sizeStr := fmt.Sprintf("%dM", size/(1024*1024))

		object := fmt.Sprintf("-object memory-backend-file,size=%s,id=%s,mem-path=%s,share=off", sizeStr, memId, path)
		device := fmt.Sprintf("-device virtio-pmem-pci,id=%s,memdev=%s,bus=pcie.0,addr=%s", devId, memId, addr)

		devices = append(devices, object, device)
	}

	return strings.Join(devices, " \\\n")
}

func getFileSize(path string) int64 {
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

	// Hand-written: part of the handshake that executeQMPCommand depends on (would recurse).
	qmpCapabilities := `{"execute": "qmp_capabilities"}
`
	_, err = conn.Write([]byte(qmpCapabilities))
	if err != nil {
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

// executeQMPCommand runs a QMP command, discarding the response payload.
func (s *StratovirtClient) executeQMPCommand(command string, arguments map[string]interface{}) error {
	_, err := s.executeQMPCommandWithResponse(command, arguments)
	return err
}

// executeQMPCommandWithResponse is the single place that sends QMP and parses the reply;
// callers should use it (or executeQMPCommand) instead of hand-rolling JSON.
func (s *StratovirtClient) executeQMPCommandWithResponse(command string, arguments map[string]interface{}) (map[string]interface{}, error) {
	logger := ulog.GetLogger()

	conn, reader, err := s.connectQMP()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	cmd := map[string]interface{}{
		"execute": command,
	}
	if arguments != nil {
		cmd["arguments"] = arguments
	}

	jsonCmd, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal qmp command: %w", err)
	}

	jsonCmdStr := string(jsonCmd) + "\n"
	logger.Debug("Sending QMP command", ulog.F("command", jsonCmdStr))

	if _, err = conn.Write([]byte(jsonCmdStr)); err != nil {
		return nil, fmt.Errorf("failed to send qmp command: %w", err)
	}

	resp, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read qmp response: %w", err)
	}

	logger.Debug("QMP response", ulog.F("response", strings.TrimSpace(resp)))

	var response map[string]interface{}
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

		if returnVal, ok := response["return"]; ok {
			if returnMap, ok := returnVal.(map[string]interface{}); ok {
				status, _ := returnMap["status"].(string)
				running, _ := returnMap["running"].(bool)
				logger.Debug("VM status check",
					ulog.F("status", status),
					ulog.F("running", running))

				if status == "paused" {
					logger.Info("VM is paused, sending cont command to start")
					err := s.executeQMPCommand("cont", nil)
					if err != nil {
						logger.Warn("Failed to send cont command", ulog.F("error", err))
						time.Sleep(100 * time.Millisecond)
						continue
					}
					time.Sleep(200 * time.Millisecond)
					continue
				}

				if status == "running" || running == true {
					logger.Info("VM is running")
					return nil
				}
			}
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

	err = s.executeQMPCommand("stop", nil)
	if err != nil {
		return err
	}

	// wait for the VM to enter the paused state
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

	if returnVal, ok := response["return"]; ok {
		if returnMap, ok := returnVal.(map[string]interface{}); ok {
			status, _ := returnMap["status"].(string)
			return status, nil
		}
	}
	return "", fmt.Errorf("invalid query-status response")
}

// Snapshot completion time scales with guest RAM size, so poll on a generous deadline.
const (
	snapshotPollInterval = 500 * time.Millisecond
	snapshotPollTimeout  = 5 * time.Minute
)

func (s *StratovirtClient) CreateSnapshot(snapfilePath string) error {
	logger := ulog.GetLogger()
	logger.Info("Creating snapshot (Stratovirt)",
		ulog.F("path", snapfilePath),
	)

	// VM should already be paused by caller (sandbox.Pause -> process.Pause -> PauseVM)
	// Just verify and proceed with migrate

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

	args := map[string]interface{}{
		"uri": "file:" + snapfilePath,
	}

	err = s.executeQMPCommand("migrate", args)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	deadline := time.Now().Add(snapshotPollTimeout)
	for time.Now().Before(deadline) {
		response, err := s.executeQMPCommandWithResponse("query-migrate", nil)
		if err != nil {
			time.Sleep(snapshotPollInterval)
			continue
		}

		if returnVal, ok := response["return"]; ok {
			if returnMap, ok := returnVal.(map[string]interface{}); ok {
				status, _ := returnMap["status"].(string)
				logger.Debug("Snapshot status", ulog.F("status", status))
				if status == "completed" {
					logger.Info("Snapshot completed successfully")
					// Write config.json so the snapshotter create/resume paths can read it; see generateSnapshotConfig.
					if err := s.generateSnapshotConfig(snapfilePath); err != nil {
						return fmt.Errorf("failed to generate snapshot config: %w", err)
					}
					return nil
				}
				if status == "failed" {
					return fmt.Errorf("snapshot failed")
				}
			}
		}

		time.Sleep(snapshotPollInterval)
	}

	return fmt.Errorf("snapshot timeout after %v", snapshotPollTimeout)
}

// generateSnapshotConfig writes a config.json the StratoVirt VM never reads, but the VMM-agnostic
// snapshotter requires it to exist on its create/resume paths (it reads SnapDir/config.json there).
func (s *StratovirtClient) generateSnapshotConfig(snapfilePath string) error {
	logger := ulog.GetLogger()

	if s.config == nil {
		return fmt.Errorf("snapshot config not available")
	}

	config := map[string]interface{}{
		"payload": map[string]interface{}{
			"kernel":    s.config.kernelPath,
			"initramfs": s.config.initrdPath,
		},
		"memory": map[string]interface{}{
			"size": s.config.memorySize * 1024 * 1024,
			"zones": []map[string]interface{}{
				{
					"id":     "mem0",
					"size":   fmt.Sprintf("%dM", s.config.memorySize),
					"shared": true,
				},
			},
		},
		"vsock": map[string]interface{}{
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
