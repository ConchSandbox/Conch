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
	DefaultRamMB       = 256
	defaultRamMB       = DefaultRamMB
	createSandbox      = "/api/sandbox/create"
	pauseSandbox       = "/api/sandbox/pause"
	pullImage          = "/api/image/pull"
	pushImage          = "/api/image/push"
	listImages         = "/api/image/list"
	removeImage        = "/api/image/remove"
	unpackImage        = "/api/image/unpack"
	convertImage       = "/api/image/convert"
	listSnapshots      = "/api/snapshot/list"
	removeSnapshot     = "/api/snapshot/remove"
	snapshotExport     = "/api/snapshot/export"
	defaultHTTPTimeout = 120 * time.Second
)

// ResolveBaseURL returns conchd base URL: CONCH_API_URL, or http://CONCHD_HOST:CONCHD_PORT (default port 4063), or DefaultConchAPIURL.
func ResolveBaseURL() string {
	baseURL, _ := resolveClientTransport("", "", 0)
	return baseURL
}

// CreateRequest matches Conch SandboxCreateRequest (image_name for image-based startup).
type CreateRequest struct {
	Namespace  string `json:"namespace,omitempty"`
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
	ImageName string `json:"image_name"`
	Namespace string `json:"namespace,omitempty"`
	PlainHTTP bool   `json:"plain_http,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
}

// UnpackImageRequest matches POST /api/image/unpack.
type UnpackImageRequest struct {
	ImageName string `json:"image_name"`
	Namespace string `json:"namespace,omitempty"`
}

type ImageResponse struct {
	Results map[string]string `json:"results"`
}

type ListImagesRequest struct {
	Namespace string   `json:"namespace,omitempty"`
	Filters   []string `json:"filters,omitempty"`
}

type ImageRecord struct {
	Name            string            `json:"name"`
	TargetDigest    string            `json:"target_digest"`
	TargetMediaType string            `json:"target_media_type"`
	Size            int64             `json:"size,omitempty"`
	Kind            string            `json:"kind,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	CreatedAt       time.Time         `json:"created_at,omitempty"`
	UpdatedAt       time.Time         `json:"updated_at,omitempty"`
}

type ListImagesResponse struct {
	Images []ImageRecord `json:"images"`
}

type RemoveImageRequest struct {
	ImageName   string `json:"image_name"`
	Namespace   string `json:"namespace,omitempty"`
	Synchronous bool   `json:"synchronous,omitempty"`
}

type ConvertImageRequest struct {
	Source       string
	KernelPath   string
	InitrdPath   string
	BootIndexTag string
	Namespace    string
	PlainHTTP    bool
	Username     string
	Password     string
	Snapshot     bool
}

type ConvertImageMetadata struct {
	Source       string `json:"source"`
	Namespace    string `json:"namespace,omitempty"`
	BootIndexTag string `json:"boot_index_tag"`
	PlainHTTP    bool   `json:"plain_http,omitempty"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	Snapshot     bool   `json:"snapshot,omitempty"`
}

type ConvertImageResponse struct {
	BootIndexDigest string `json:"boot_index_digest"`
	BootIndexTag    string `json:"boot_index_tag"`
	RootfsImageRef  string `json:"rootfs_image_ref,omitempty"`
	SourceImageRef  string `json:"source_image_ref,omitempty"`
}

type SnapshotExportRequest struct {
	Namespace        string `json:"namespace,omitempty"`
	BootIndexTag     string `json:"boot_index_tag"`
	RootfsSnapshotID string `json:"snapshot_id,omitempty"`
	SandboxID        string `json:"sandbox_id,omitempty"`
}

type SnapshotExportResponse struct {
	BootIndexDigest string `json:"boot_index_digest"`
	BootIndexTag    string `json:"boot_index_tag"`
}

type ListSnapshotsRequest struct {
	Namespace string   `json:"namespace,omitempty"`
	Filters   []string `json:"filters,omitempty"`
}

type SnapshotRecord struct {
	Key         string            `json:"key"`
	Kind        string            `json:"kind,omitempty"`
	ConchRole   string            `json:"conch_role,omitempty"`
	GroupID     string            `json:"group_id,omitempty"`
	Parent      string            `json:"parent,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	StoragePath string            `json:"storage_path,omitempty"`
	CreatedAt   time.Time         `json:"created_at,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at,omitempty"`
}

type ListSnapshotsResponse struct {
	Snapshots []SnapshotRecord `json:"snapshots"`
}

type RemoveSnapshotRequest struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
	Cascade   bool   `json:"cascade,omitempty"`
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
	resolvedURL, httpClient := resolveClientTransport(baseURL, configPath, 0)
	return &Client{
		baseURL:    resolvedURL,
		httpClient: httpClient,
	}
}

// NewClientWithConfigAndTimeout creates a Conch API client with an optional
// per-call HTTP timeout override.
func NewClientWithConfigAndTimeout(baseURL, configPath string, timeoutOverride time.Duration) *Client {
	resolvedURL, httpClient := resolveClientTransport(baseURL, configPath, timeoutOverride)
	return &Client{
		baseURL:    resolvedURL,
		httpClient: httpClient,
	}
}

func resolveClientTransport(baseURL, configPath string, timeoutOverride time.Duration) (string, *http.Client) {
	timeout := timeoutOverride
	if timeout <= 0 {
		timeout = resolveHTTPTimeout()
	}
	if strings.TrimSpace(baseURL) != "" {
		return baseURL, &http.Client{Timeout: timeout}
	}

	if u := strings.TrimSpace(os.Getenv("CONCH_API_URL")); u != "" {
		return u, &http.Client{Timeout: timeout}
	}

	cfgPath := configPath
	if cfgPath == "" {
		cfgPath = config.FindConfigFile()
	}
	if cfg, err := config.LoadConfig(cfgPath); err == nil {
		if unixSocket := strings.TrimSpace(cfg.GetServerUnixSocket()); unixSocket != "" {
			return defaultUnixAPIURL, newUnixSocketHTTPClient(unixSocket, timeout)
		}
		host := strings.TrimSpace(cfg.Server.Host)
		port := cfg.Server.Port
		if host != "" && port > 0 {
			return fmt.Sprintf("http://%s:%d", host, port), &http.Client{Timeout: timeout}
		}
	}

	host := strings.TrimSpace(os.Getenv("CONCHD_HOST"))
	port := strings.TrimSpace(os.Getenv("CONCHD_PORT"))
	if host != "" {
		if port == "" {
			port = "4063"
		}
		return fmt.Sprintf("http://%s:%s", host, port), &http.Client{Timeout: timeout}
	}

	return DefaultConchAPIURL, &http.Client{Timeout: timeout}
}

type PushImageRequest struct {
	LocalImage      string `json:"local_image"`
	RemoteImage     string `json:"remote_image"`
	Namespace       string `json:"namespace,omitempty"`
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	RegistryTimeout string `json:"registry_timeout,omitempty"`
}

func resolveHTTPTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CONCH_API_TIMEOUT"))
	if raw == "" {
		return defaultHTTPTimeout
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return defaultHTTPTimeout
	}
	return timeout
}

func newUnixSocketHTTPClient(socketPath string, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// CreateSandbox calls POST /api/sandbox/create using image_name-based startup.
func (c *Client) CreateSandbox(rootfsImageName, sandboxID, namespace string, ramMB int64) error {
	if ramMB <= 0 {
		ramMB = defaultRamMB
	}
	req := CreateRequest{
		Namespace: strings.TrimSpace(namespace),
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

func (c *Client) PushImage(ctx context.Context, req PushImageRequest) error {
	var resp map[string]string
	return c.postJSON(ctx, pushImage, req, &resp)
}

func (c *Client) ListImages(ctx context.Context, req ListImagesRequest) ([]ImageRecord, error) {
	var resp ListImagesResponse
	if err := c.postJSON(ctx, listImages, req, &resp); err != nil {
		return nil, err
	}
	return resp.Images, nil
}

func (c *Client) RemoveImage(ctx context.Context, req RemoveImageRequest) error {
	var resp map[string]string
	return c.postJSON(ctx, removeImage, req, &resp)
}

// UnpackImage calls POST /api/image/unpack and returns snapshot IDs by component kind.
func (c *Client) UnpackImage(ctx context.Context, req UnpackImageRequest) (map[string]string, error) {
	var resp ImageResponse
	if err := c.postJSON(ctx, unpackImage, req, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

func (c *Client) ConvertImage(ctx context.Context, req ConvertImageRequest) (ConvertImageResponse, error) {
	if req.KernelPath == "" {
		return ConvertImageResponse{}, fmt.Errorf("kernel path is required")
	}
	if req.InitrdPath == "" {
		return ConvertImageResponse{}, fmt.Errorf("initrd path is required")
	}
	kernel, err := os.Open(req.KernelPath)
	if err != nil {
		return ConvertImageResponse{}, fmt.Errorf("open kernel: %w", err)
	}
	defer kernel.Close()
	initrd, err := os.Open(req.InitrdPath)
	if err != nil {
		return ConvertImageResponse{}, fmt.Errorf("open initrd: %w", err)
	}
	defer initrd.Close()

	metadata, err := json.Marshal(ConvertImageMetadata{
		Source:       req.Source,
		Namespace:    req.Namespace,
		BootIndexTag: req.BootIndexTag,
		PlainHTTP:    req.PlainHTTP,
		Username:     req.Username,
		Password:     req.Password,
		Snapshot:     req.Snapshot,
	})
	if err != nil {
		return ConvertImageResponse{}, fmt.Errorf("marshaling convert metadata: %w", err)
	}
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
		if err = writer.WriteField("metadata", string(metadata)); err != nil {
			return
		}
		var part io.Writer
		part, err = writer.CreateFormFile("kernel", filepath.Base(req.KernelPath))
		if err != nil {
			return
		}
		if _, err = io.Copy(part, kernel); err != nil {
			return
		}
		part, err = writer.CreateFormFile("initrd", filepath.Base(req.InitrdPath))
		if err != nil {
			return
		}
		if _, err = io.Copy(part, initrd); err != nil {
			return
		}
		err = writer.Close()
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+convertImage, pr)
	if err != nil {
		_ = pr.Close()
		return ConvertImageResponse{}, fmt.Errorf("create request %s: %w", convertImage, err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return ConvertImageResponse{}, fmt.Errorf("POST %s: %w", convertImage, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return ConvertImageResponse{}, fmt.Errorf("%s returned status %d: %s", convertImage, resp.StatusCode, string(body))
	}
	var out ConvertImageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ConvertImageResponse{}, fmt.Errorf("decoding %s response: %w", convertImage, err)
	}
	return out, nil
}

func (c *Client) ExportSnapshot(ctx context.Context, req SnapshotExportRequest) (SnapshotExportResponse, error) {
	var out SnapshotExportResponse
	if err := c.postJSON(ctx, snapshotExport, req, &out); err != nil {
		return SnapshotExportResponse{}, err
	}
	return out, nil
}

func (c *Client) ListSnapshots(ctx context.Context, req ListSnapshotsRequest) ([]SnapshotRecord, error) {
	var resp ListSnapshotsResponse
	if err := c.postJSON(ctx, listSnapshots, req, &resp); err != nil {
		return nil, err
	}
	return resp.Snapshots, nil
}

func (c *Client) RemoveSnapshot(ctx context.Context, req RemoveSnapshotRequest) error {
	var resp map[string]string
	return c.postJSON(ctx, removeSnapshot, req, &resp)
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
