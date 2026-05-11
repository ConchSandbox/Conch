package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
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
	pullImage          = "/api/image/pull"
	unpackImage        = "/api/image/unpack"
	importImage        = "/api/image/import"
	linkSnapshotVM     = "/api/snapshot/link-vm"
	snapshotInfo       = "/api/snapshot/info"
	snapshotChain      = "/api/snapshot/chain"
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
	Namespace string `json:"namespace,omitempty"`
	SandboxId string `json:"sandbox_id"`
}

// PauseResponse is the JSON response from sandbox pause
type PauseResponse struct {
	Status     string `json:"status"`
	SnapshotId string `json:"snapshotId"` // Conch returns camelCase key
}

// PullImageRequest matches POST /api/image/pull.
type PullImageRequest struct {
	ImageName          string `json:"image_name"`
	Namespace          string `json:"namespace,omitempty"`
	PlainHTTP          bool   `json:"plain_http,omitempty"`
	Username           string `json:"username,omitempty"`
	Password           string `json:"password,omitempty"`
	DefaultKernelImage string `json:"default_kernel_image,omitempty"`
	KernelPlainHTTP    bool   `json:"kernel_plain_http,omitempty"`
	KernelUsername     string `json:"kernel_username,omitempty"`
	KernelPassword     string `json:"kernel_password,omitempty"`
}

// UnpackImageRequest matches POST /api/image/unpack.
type UnpackImageRequest struct {
	ImageName string `json:"image_name"`
	Namespace string `json:"namespace,omitempty"`
}

type ImageResponse struct {
	Results map[string]string `json:"results"`
}

type ImportImageRequest struct {
	ArchivePath string
	Namespace   string
	ImportedTag string
}

type ImportImageResponse struct {
	SnapshotKey string `json:"snapshot_key"`
	ImageName   string `json:"image_name"`
}

type LinkSnapshotVMRequest struct {
	RootfsSnapshotID string `json:"rootfs_snapshot_id"`
	VMSnapshotID     string `json:"vm_snapshot_id"`
	Namespace        string `json:"namespace,omitempty"`
}

type SnapshotInfoRequest struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
}

type SnapshotMeta struct {
	Key         string            `json:"key"`
	Parent      string            `json:"parent,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	StoragePath string            `json:"storage_path,omitempty"`
}

type SnapshotChainResponse struct {
	Info       SnapshotMeta `json:"info"`
	ChainPaths []string     `json:"chain_paths"`
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

// CreateSandbox calls POST /api/sandbox/create using image_name-based startup.
func (c *Client) CreateSandbox(rootfsImageName, sandboxID string, ramMB int64) error {
	if ramMB <= 0 {
		ramMB = defaultRamMB
	}
	req := CreateRequest{
		ImageName: rootfsImageName,
		SandboxId: sandboxID,
		VmmName:   defaultVmmName,
		VcpuNum:   1,
		RamMB:     ramMB,
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
func (c *Client) PauseSandbox(sandboxID, namespace string) (string, error) {
	req := PauseRequest{
		Namespace: strings.TrimSpace(namespace),
		SandboxId: sandboxID,
	}
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

// PullImage calls POST /api/image/pull and returns snapshot IDs by component kind.
func (c *Client) PullImage(ctx context.Context, req PullImageRequest) (map[string]string, error) {
	var resp ImageResponse
	if err := c.postJSON(ctx, pullImage, req, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// UnpackImage calls POST /api/image/unpack and returns snapshot IDs by component kind.
func (c *Client) UnpackImage(ctx context.Context, req UnpackImageRequest) (map[string]string, error) {
	var resp ImageResponse
	if err := c.postJSON(ctx, unpackImage, req, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

func (c *Client) ImportImageArchive(ctx context.Context, req ImportImageRequest) (ImportImageResponse, error) {
	if req.ArchivePath == "" {
		return ImportImageResponse{}, fmt.Errorf("archive path is required")
	}
	file, err := os.Open(req.ArchivePath)
	if err != nil {
		return ImportImageResponse{}, fmt.Errorf("open archive: %w", err)
	}
	defer file.Close()

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		var err error
		defer func() {
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			_ = pw.Close()
		}()
		if req.Namespace != "" {
			if err = writer.WriteField("namespace", req.Namespace); err != nil {
				return
			}
		}
		if req.ImportedTag != "" {
			if err = writer.WriteField("imported_tag", req.ImportedTag); err != nil {
				return
			}
		}
		var part io.Writer
		part, err = writer.CreateFormFile("archive", filepath.Base(req.ArchivePath))
		if err != nil {
			return
		}
		if _, err = io.Copy(part, file); err != nil {
			return
		}
		err = writer.Close()
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+importImage, pr)
	if err != nil {
		_ = pr.Close()
		return ImportImageResponse{}, fmt.Errorf("create request %s: %w", importImage, err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return ImportImageResponse{}, fmt.Errorf("POST %s: %w", importImage, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return ImportImageResponse{}, fmt.Errorf("%s returned status %d: %s", importImage, resp.StatusCode, string(body))
	}
	var out ImportImageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ImportImageResponse{}, fmt.Errorf("decoding %s response: %w", importImage, err)
	}
	return out, nil
}

func (c *Client) LinkRootfsSnapshotToVM(ctx context.Context, req LinkSnapshotVMRequest) error {
	return c.postJSON(ctx, linkSnapshotVM, req, &struct {
		Status string `json:"status"`
	}{})
}

func (c *Client) SnapshotInfo(ctx context.Context, req SnapshotInfoRequest) (SnapshotMeta, error) {
	var out SnapshotMeta
	if err := c.postJSON(ctx, snapshotInfo, req, &out); err != nil {
		return SnapshotMeta{}, err
	}
	return out, nil
}

func (c *Client) SnapshotChain(ctx context.Context, req SnapshotInfoRequest) (SnapshotChainResponse, error) {
	var out SnapshotChainResponse
	if err := c.postJSON(ctx, snapshotChain, req, &out); err != nil {
		return SnapshotChainResponse{}, err
	}
	return out, nil
}

func (c *Client) postJSON(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s returned status %d: %s", path, resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s response: %w", path, err)
	}
	return nil
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
