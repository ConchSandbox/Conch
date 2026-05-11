package vmm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/template"

	"github.com/openeuler/Conch/pkg/ulog"
)

const defaultVmmBinary = "/usr/local/bin/cloud-hypervisor"

const startScriptCLH = `ip netns exec {{ .NamespaceID }} \
{{ .VmmBinaryPath }} \
--cpus boot={{ .CPUBoot }},max={{ .CPUMax }},max_phys_bits=42 \
--kernel {{ .KernelPath }} \
--initramfs {{ .InitrdPath }} \
--pmem file={{ .PmemPath }},discard_writes=on \
--memory "size=0" \
--memory-zone "id=mem0,size={{ .MemorySize }},file={{ .MemoryPath }},shared=on" \
--cmdline "console=hvc0 root=/dev/ram0 rw debug conch.sandbox_id={{ .SandboxId }}" \
--api-socket {{ .VmmSocket }} \
--console null \
--net "tap={{ .TapName }}" \
--vsock "cid={{ .VsockCID }},socket={{ .VsockSocketPath }}" \
--seccomp false`

// -vv use for printing log when test
// Current VM lifecycle is bound to Conch; Conch exit causes VM process termination. Detachment needed for follow-up.

const resumeScriptCLH = `ip netns exec {{ .NamespaceID }} \
{{ .VmmBinaryPath }} --api-socket \
{{ .VmmSocket }} \
--seccomp false`

type StartScriptCLHArgs struct {
	VmmBinaryPath   string
	CPUBoot         int64
	CPUMax          int64
	MemorySize      string
	MemoryPath      string
	KernelPath      string
	InitrdPath      string
	PmemPath        string
	NamespaceID     string
	TapName         string
	VmmSocket       string
	VsockCID        uint32
	VsockSocketPath string
	SandboxId       string
}

type CLHClient struct {
	vmmType    int
	socketPath string
}

func NewCLHClient(vmmType int, socketPath string) *CLHClient {
	return &CLHClient{
		vmmType:    vmmType,
		socketPath: socketPath,
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

func (clh *CLHClient) BuildStartCmd(args *ResourceArgs, isResume bool) (string, error) {
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
		PmemPath:        strings.Join(args.PmemPaths, " file="),
		NamespaceID:     args.NamespaceID,
		TapName:         args.TapName,
		VmmSocket:       clh.socketPath,
		VsockCID:        args.VsockCID,
		VsockSocketPath: args.VsockSocketPath,
		SandboxId:       args.SandboxId,
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
