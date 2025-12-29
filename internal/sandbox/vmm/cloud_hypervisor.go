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
	"strconv"
	"text/template"
)

const defaultVmmBinary = "/usr/local/bin/cloud-hypervisor"

const startScriptCLH = `ip netns exec {{ .NamespaceID }} \
{{ .VmmBinaryPath }} \
--cpus boot={{ .CPUBoot }},max={{ .CPUMax }} \
--kernel {{ .KernelPath }} \
--disk path={{ .DiskPath }} \
--memory "size=0" \
--memory-zone "id=mem0,size={{ .MemorySize }},file={{ .MemoryPath }},shared=on" \
--cmdline "console=hvc0 root=/dev/vda1 rw debug" \
--api-socket {{ .VmmSocket }} \
--console off \
--net "tap={{ .TapName }}" \
--seccomp false`

// -vv use for printing log when test
// Current VM lifecycle is bound to Conch; Conch exit causes VM process termination. Detachment needed for follow-up.

const resumeScriptCLH = `ip netns exec {{ .NamespaceID }} \
{{ .VmmBinaryPath }} --api-socket \
{{ .VmmSocket }} \
-vv \
--seccomp false`

type StartScriptCLHArgs struct {
	VmmBinaryPath string
	CPUBoot       int64
	CPUMax        int64
	MemorySize    string
	MemoryPath    string
	KernelPath    string
	DiskPath      string
	NamespaceID   string
	TapName       string
	VmmSocket     string
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

func (clh *CLHClient) BuildStartCmd(args *ResourceArgs, rootfsSock, kernelPath string, diskPath string, isResume bool) (string, error) {
	clhArgs := StartScriptCLHArgs{
		VmmBinaryPath: defaultVmmBinary,
		CPUBoot:       args.CPUBoot,
		CPUMax:        args.CPUMax,
		MemorySize:    strconv.FormatInt(args.MemorySize, 10) + "M",
		MemoryPath:    args.MemoryPath,
		KernelPath:    kernelPath,
		DiskPath:      diskPath,
		NamespaceID:   args.NamespaceID,
		TapName:       args.TapName,
		VmmSocket:     clh.socketPath,
	}

	_, err := os.Stat(clhArgs.VmmBinaryPath)
	if err != nil {
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
		return "", fmt.Errorf("error executing fc start script template: %w", err)
	}

	// debug
	script := scriptBuffer.String()
	fmt.Printf("Build cmd: %s\n", script)

	return script, nil
}

func (c *CLHClient) requestApi(method, fullCommand, requestBody string) error {
	request := buildRequest(method, fullCommand, requestBody)
	fmt.Printf("request:%s\n", request)
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("failed to connect to socket: %w", err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte(request))
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return fmt.Errorf("failed to parse HTTP response: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	if isServerError(resp.StatusCode) {
		return fmt.Errorf("server returned error: %s", resp.Status)
	}

	fmt.Printf("%s\n", string(body))
	return nil
}

func (c *CLHClient) CheckDaemonAlive() error {
	// TODO: call conchd GetHealth
	return nil
}

func (c *CLHClient) PauseVM() error {
	return c.requestApi("PUT", "pause", "")
}

func (c *CLHClient) ResumeVM() error {
	return c.requestApi("PUT", "resume", "")
}

func (c *CLHClient) DeleteVM() error {
	return c.requestApi("PUT", "delete", "")
}

func (c *CLHClient) CreateSnapshot(snapfilePath string) error {
	requestBody := struct {
		DestinationURL string `json:"destination_url"`
	}{
		DestinationURL: "file://" + snapfilePath,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return c.requestApi("PUT", "snapshot", string(jsonBody))
}

func (c *CLHClient) LoadSnapshot(snapfilePath string, prefault bool) error {
	requestBody := struct {
		SourceURL string `json:"source_url"`
		Prefault  bool   `json:"prefault"`
	}{
		SourceURL: "file://" + snapfilePath,
		Prefault:  prefault,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return c.requestApi("PUT", "restore", string(jsonBody))
}