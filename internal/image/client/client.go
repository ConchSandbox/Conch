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
	suspendSandbox     = "/api/sandbox/suspend"
	resumeSandbox      = "/api/sandbox/resume"
	checkpointSandbox  = "/api/sandbox/checkpoint"
	createTemplate     = "/api/template/create"
	pullTemplate       = "/api/template/pull"
	pushTemplate       = "/api/template/push"
	listTemplates      = "/api/template/list"
	inspectTemplate    = "/api/template/inspect"
	removeTemplate     = "/api/template/remove"
	pullImage          = "/api/image/pull"
	pushImage          = "/api/image/push"
	listImages         = "/api/image/list"
	removeImage        = "/api/image/remove"
	unpackImage        = "/api/image/unpack"
	listSnapshots      = "/api/snapshot/list"
	removeSnapshot     = "/api/snapshot/remove"
	defaultHTTPTimeout = 120 * time.Second
)

// ResolveBaseURL returns conchd base URL: CONCH_API_URL, or http://CONCHD_HOST:CONCHD_PORT (default port 4063), or DefaultConchAPIURL.
func ResolveBaseURL() string {
	baseURL, _ := resolveClientTransport("", "", 0)
	return baseURL
}

// CreateRequest matches Conch SandboxCreateRequest.
type CreateRequest struct {
	Namespace    string        `json:"namespace,omitempty"`
	TemplateID   string        `json:"template_id"`
	VmmName      string        `json:"vmm_name"`
	SandboxId    string        `json:"sandbox_id"`
	VcpuNum      int64         `json:"vcpu_num"`
	RamMB        int64         `json:"ram_mb"`
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`
}

type VolumeMount struct {
	Source   string `json:"source"`
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

// CreateResponse is the JSON response from sandbox create
type CreateResponse struct {
	Status string `json:"status"`
	IP     string `json:"ip"`
}

type SandboxLifecycleRequest struct {
	Namespace string `json:"namespace,omitempty"`
	SandboxId string `json:"sandbox_id"`
}

type SandboxCheckpointRequest struct {
	Namespace string            `json:"namespace,omitempty"`
	SandboxId string            `json:"sandbox_id"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type SandboxCheckpointResponse struct {
	Status     string `json:"status"`
	TemplateID string `json:"template_id"`
}

type TemplateIDRequest struct {
	ID string `json:"id"`
}

type TemplateListRequest struct {
	Namespace string `json:"namespace,omitempty"`
	Origin    string `json:"origin,omitempty"`
	BootMode  string `json:"boot_mode,omitempty"`
}

type TemplateRecord struct {
	ID               string            `json:"id"`
	Origin           string            `json:"origin"`
	BootMode         string            `json:"boot_mode"`
	BootIndexDigest  string            `json:"boot_index_digest,omitempty"`
	Namespace        string            `json:"namespace"`
	State            string            `json:"state"`
	ParentTemplateID string            `json:"parent_template_id,omitempty"`
	SourceSandboxID  string            `json:"source_sandbox_id,omitempty"`
	ImageName        string            `json:"image_name,omitempty"`
	BuildRef         string            `json:"build_ref,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	CreatedAt        int64             `json:"created_at,omitempty"`
	UpdatedAt        int64             `json:"updated_at,omitempty"`
	LastError        string            `json:"last_error,omitempty"`
}

type TemplateListResponse struct {
	Items []TemplateRecord `json:"items"`
}

type TemplatePullRequest struct {
	Reference string            `json:"reference"`
	Namespace string            `json:"namespace,omitempty"`
	PlainHTTP bool              `json:"plain_http,omitempty"`
	Username  string            `json:"username,omitempty"`
	Password  string            `json:"password,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type TemplatePullResponse struct {
	Status          string `json:"status"`
	TemplateID      string `json:"template_id"`
	BootIndexDigest string `json:"boot_index_digest"`
	BuildRef        string `json:"build_ref"`
}

type TemplatePushRequest struct {
	TemplateID      string `json:"template_id"`
	RemoteReference string `json:"remote_reference"`
	Namespace       string `json:"namespace,omitempty"`
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	RegistryTimeout string `json:"registry_timeout,omitempty"`
}

// PullImageRequest matches POST /api/image/pull.
type PullImageRequest struct {
	ImageName  string `json:"image_name"`
	Namespace  string `json:"namespace,omitempty"`
	PlainHTTP  bool   `json:"plain_http,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	SkipUnpack bool   `json:"skip_unpack,omitempty"`
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

type TemplateCreateRequest struct {
	Source       string
	KernelPath   string
	InitrdPath   string
	BootIndexTag string
	Namespace    string
	PlainHTTP    bool
	Username     string
	Password     string
	Labels       map[string]string
}

type TemplateCreateMetadata struct {
	Source       string            `json:"source"`
	Namespace    string            `json:"namespace,omitempty"`
	BootIndexTag string            `json:"boot_index_tag,omitempty"`
	PlainHTTP    bool              `json:"plain_http,omitempty"`
	Username     string            `json:"username,omitempty"`
	Password     string            `json:"password,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

type TemplateCreateResponse struct {
	Status          string `json:"status,omitempty"`
	TemplateID      string `json:"template_id"`
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

// CreateSandbox calls POST /api/sandbox/create using a template ID.
func (c *Client) CreateSandbox(templateID, sandboxID, namespace string, ramMB int64) error {
	if ramMB <= 0 {
		ramMB = defaultRamMB
	}
	req := CreateRequest{
		Namespace:  strings.TrimSpace(namespace),
		TemplateID: templateID,
		SandboxId:  sandboxID,
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

func (c *Client) SuspendSandbox(ctx context.Context, sandboxID, namespace string) error {
	var resp map[string]string
	return c.postJSON(ctx, suspendSandbox, SandboxLifecycleRequest{Namespace: strings.TrimSpace(namespace), SandboxId: sandboxID}, &resp)
}

func (c *Client) ResumeSandbox(ctx context.Context, sandboxID, namespace string) error {
	var resp map[string]string
	return c.postJSON(ctx, resumeSandbox, SandboxLifecycleRequest{Namespace: strings.TrimSpace(namespace), SandboxId: sandboxID}, &resp)
}

func (c *Client) CheckpointSandbox(ctx context.Context, sandboxID, namespace string) (string, error) {
	var resp SandboxCheckpointResponse
	if err := c.postJSON(ctx, checkpointSandbox, SandboxCheckpointRequest{Namespace: strings.TrimSpace(namespace), SandboxId: sandboxID}, &resp); err != nil {
		return "", err
	}
	if resp.Status != "ok" {
		return "", fmt.Errorf("checkpoint status: %s", resp.Status)
	}
	return resp.TemplateID, nil
}

func (c *Client) ListTemplates(ctx context.Context, req TemplateListRequest) ([]TemplateRecord, error) {
	var resp TemplateListResponse
	req.Namespace = strings.TrimSpace(req.Namespace)
	req.Origin = strings.TrimSpace(req.Origin)
	req.BootMode = strings.TrimSpace(req.BootMode)
	if err := c.postJSON(ctx, listTemplates, req, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) InspectTemplate(ctx context.Context, id string) (TemplateRecord, error) {
	var resp TemplateRecord
	if err := c.postJSON(ctx, inspectTemplate, TemplateIDRequest{ID: id}, &resp); err != nil {
		return TemplateRecord{}, err
	}
	return resp, nil
}

func (c *Client) RemoveTemplate(ctx context.Context, id string) error {
	var resp map[string]string
	return c.postJSON(ctx, removeTemplate, TemplateIDRequest{ID: id}, &resp)
}

func (c *Client) PullTemplate(ctx context.Context, req TemplatePullRequest) (TemplatePullResponse, error) {
	var resp TemplatePullResponse
	if err := c.postJSON(ctx, pullTemplate, req, &resp); err != nil {
		return TemplatePullResponse{}, err
	}
	return resp, nil
}

func (c *Client) PushTemplate(ctx context.Context, req TemplatePushRequest) error {
	var resp map[string]string
	return c.postJSON(ctx, pushTemplate, req, &resp)
}

// PullImage calls POST /api/image/pull and returns snapshot IDs by component kind unless unpacking is skipped.
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

func (c *Client) CreateTemplate(ctx context.Context, req TemplateCreateRequest) (TemplateCreateResponse, error) {
	metadata, err := json.Marshal(TemplateCreateMetadata{
		Source:       req.Source,
		Namespace:    req.Namespace,
		BootIndexTag: req.BootIndexTag,
		PlainHTTP:    req.PlainHTTP,
		Username:     req.Username,
		Password:     req.Password,
		Labels:       req.Labels,
	})
	if err != nil {
		return TemplateCreateResponse{}, fmt.Errorf("marshaling template create metadata: %w", err)
	}
	var out TemplateCreateResponse
	if err := c.postBootMultipart(ctx, createTemplate, req.KernelPath, req.InitrdPath, metadata, &out); err != nil {
		return TemplateCreateResponse{}, err
	}
	return out, nil
}

func (c *Client) postBootMultipart(ctx context.Context, path, kernelPath, initrdPath string, metadata []byte, out any) error {
	if kernelPath == "" {
		return fmt.Errorf("kernel path is required")
	}
	if initrdPath == "" {
		return fmt.Errorf("initrd path is required")
	}
	kernel, err := os.Open(kernelPath)
	if err != nil {
		return fmt.Errorf("open kernel: %w", err)
	}
	defer kernel.Close()
	initrd, err := os.Open(initrdPath)
	if err != nil {
		return fmt.Errorf("open initrd: %w", err)
	}
	defer initrd.Close()

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
		part, err = writer.CreateFormFile("kernel", filepath.Base(kernelPath))
		if err != nil {
			return
		}
		if _, err = io.Copy(part, kernel); err != nil {
			return
		}
		part, err = writer.CreateFormFile("initrd", filepath.Base(initrdPath))
		if err != nil {
			return
		}
		if _, err = io.Copy(part, initrd); err != nil {
			return
		}
		err = writer.Close()
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, pr)
	if err != nil {
		_ = pr.Close()
		return fmt.Errorf("create request %s: %w", path, err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.httpClient.Do(httpReq)
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

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request %s: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
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
