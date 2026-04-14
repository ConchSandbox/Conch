package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openeuler/Conch/internal/config"
)

const (
	// DefaultConchAPIURL is the default conchd HTTP base URL.
	DefaultConchAPIURL = "http://localhost:4063"
	defaultUnixAPIURL  = "http://conchd-unix"
	defaultVmmName     = "cloud-hypervisor"
	DefaultRamMB       = 256 // Exported for SNAP CreateSandbox; override via SNAPOpts if needed
	defaultRamMB       = DefaultRamMB
	createSandbox      = "/api/sandbox/create"
	pauseSandbox       = "/api/sandbox/pause"
	requestTimeout     = 120 * time.Second
	sandboxIDPrefix    = "buildah-snap-"
)

// ResolveBaseURL returns conchd base URL: BUILDAH_CONCH_API_URL, or http://CONCHD_HOST:CONCHD_PORT (default port 4063), or DefaultConchAPIURL.
func ResolveBaseURL() string {
	baseURL, _ := resolveClientTransport("", "")
	return baseURL
}

// CreateRequest matches Conch SandboxCreateRequest (image_name for image-based startup).
type CreateRequest struct {
	SnapshotId string `json:"snapshot_id,omitempty"`
	ImageName  string `json:"image_name"`
	VmmName    string `json:"vmm_name"`
	SandboxId  string `json:"sandbox_id"`
	KernelPath string `json:"kernel_path"`
	DiskPath   string `json:"disk_image_path"`
	VcpuNum    int64  `json:"vcpu_num"`
	RamMB      int64  `json:"ram_mb"`
}

// CreateResponse is the JSON response from sandbox create
type CreateResponse struct {
	Status string `json:"status"`
	IP     string `json:"ip"`
}

// PauseRequest matches Conch SandboxPauseRequest
type PauseRequest struct {
	SandboxId string `json:"sandbox_id"`
}

// PauseResponse is the JSON response from sandbox pause
type PauseResponse struct {
	Status     string `json:"status"`
	SnapshotId string `json:"snapshotId"` // Conch returns camelCase key
}

// Client communicates with Conch conchd HTTP API
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a Conch API client. baseURL defaults to DefaultConchAPIURL if empty.
func NewClient(baseURL string) *Client {
	return NewClientWithConfig(baseURL, "")
}

// NewClientWithConfig creates a Conch API client using configPath when baseURL is empty.
func NewClientWithConfig(baseURL, configPath string) *Client {
	resolvedURL, httpClient := resolveClientTransport(baseURL, configPath)
	return &Client{
		baseURL:    resolvedURL,
		httpClient: httpClient,
	}
}

func resolveClientTransport(baseURL, configPath string) (string, *http.Client) {
	if strings.TrimSpace(baseURL) != "" {
		return baseURL, &http.Client{Timeout: requestTimeout}
	}

	if u := strings.TrimSpace(os.Getenv("BUILDAH_CONCH_API_URL")); u != "" {
		return u, &http.Client{Timeout: requestTimeout}
	}

	cfgPath := configPath
	if cfgPath == "" {
		cfgPath = config.FindConfigFile()
	}
	if cfg, err := config.LoadConfig(cfgPath); err == nil {
		if unixSocket := strings.TrimSpace(cfg.GetServerUnixSocket()); unixSocket != "" {
			return defaultUnixAPIURL, newUnixSocketHTTPClient(unixSocket)
		}
		host := strings.TrimSpace(cfg.Server.Host)
		port := cfg.Server.Port
		if host != "" && port > 0 {
			return fmt.Sprintf("http://%s:%d", host, port), &http.Client{Timeout: requestTimeout}
		}
	}

	host := strings.TrimSpace(os.Getenv("CONCHD_HOST"))
	port := strings.TrimSpace(os.Getenv("CONCHD_PORT"))
	if host != "" {
		if port == "" {
			port = "4063"
		}
		return fmt.Sprintf("http://%s:%s", host, port), &http.Client{Timeout: requestTimeout}
	}

	return DefaultConchAPIURL, &http.Client{Timeout: requestTimeout}
}

func newUnixSocketHTTPClient(socketPath string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{
		Timeout:   requestTimeout,
		Transport: transport,
	}
}

// CreateSandbox calls POST /api/sandbox/create (JSON field image_name = rootfsImageName after containerd import).
func (c *Client) CreateSandbox(rootfsImageName, sandboxID, kernelPath, diskPath string, ramMB int64) error {
	if ramMB <= 0 {
		ramMB = defaultRamMB
	}
	req := CreateRequest{
		ImageName:  rootfsImageName,
		SandboxId:  sandboxID,
		KernelPath: kernelPath,
		DiskPath:   diskPath,
		VmmName:    defaultVmmName,
		VcpuNum:    1,
		RamMB:      ramMB,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling create request: %w", err)
	}
	resp, err := c.httpClient.Post(c.baseURL+createSandbox, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST %s: %w", createSandbox, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create sandbox returned status %d: %s", resp.StatusCode, string(body))
	}
	var cr CreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return fmt.Errorf("decoding create response: %w", err)
	}
	if cr.Status != "ok" {
		return fmt.Errorf("create sandbox status: %s", cr.Status)
	}
	return nil
}

// PauseSandbox calls POST /api/sandbox/pause, returns the rootfs snapshot name (snapshotId)
func (c *Client) PauseSandbox(sandboxID string) (string, error) {
	req := PauseRequest{SandboxId: sandboxID}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshaling pause request: %w", err)
	}
	resp, err := c.httpClient.Post(c.baseURL+pauseSandbox, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", pauseSandbox, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("pause sandbox returned status %d: %s", resp.StatusCode, string(body))
	}
	var pr PauseResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return "", fmt.Errorf("decoding pause response: %w", err)
	}
	if pr.Status != "ok" {
		return "", fmt.Errorf("pause sandbox status: %s", pr.Status)
	}
	return pr.SnapshotId, nil
}

// GenSandboxID returns a unique sandbox ID for buildah SNAP
func GenSandboxID() string {
	return sandboxIDPrefix + fmt.Sprintf("%d", time.Now().UnixNano())
}

// ResolveKernelPaths resolves kernel and initrd paths from filenames under contextDir.
// KERNEL kernel_file initrd_file - both files must exist in the Dockerfile context directory.
func ResolveKernelPaths(contextDir string, kernelFile, initrdFile string) (kernelPath, diskPath string, err error) {
	if kernelFile == "" || initrdFile == "" {
		return "", "", fmt.Errorf("KERNEL instruction requires exactly two arguments: kernel filename and initrd filename (e.g. KERNEL vmlinuz conch.initrd)")
	}
	absContext, err := filepath.Abs(contextDir)
	if err != nil {
		return "", "", fmt.Errorf("context directory: %w", err)
	}
	kernelPath = filepath.Clean(filepath.Join(absContext, kernelFile))
	diskPath = filepath.Clean(filepath.Join(absContext, initrdFile))

	// Ensure paths stay within context (prevent path traversal)
	for name, p := range map[string]string{"kernel": kernelPath, "initrd": diskPath} {
		rel, relErr := filepath.Rel(absContext, p)
		if relErr != nil || strings.HasPrefix(rel, "..") {
			return "", "", fmt.Errorf("KERNEL file path escapes context: %s", name)
		}
		if _, statErr := os.Stat(p); statErr != nil {
			if os.IsNotExist(statErr) {
				return "", "", fmt.Errorf("KERNEL file not found in context: %s (expected at %s)", name, p)
			}
			return "", "", fmt.Errorf("cannot access %s: %w", p, statErr)
		}
	}
	return kernelPath, diskPath, nil
}
