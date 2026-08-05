package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

const (
	createSandbox     = "/api/v1/sandboxes"
	listSandbox       = "/api/v1/sandboxes?limit=5000"
	deleteSandbox     = "/api/v1/sandboxes/"
	suspendSandbox    = "/api/sandbox/suspend"
	resumeSandbox     = "/api/sandbox/resume"
	checkpointSandbox = "/api/sandbox/checkpoint"
)

type SandboxCreateRequest struct {
	TemplateID string `json:"template_id,omitempty"`
	VMMName    string `json:"vmm_name,omitempty"`
	SandboxID  string `json:"sandbox_id"`
	VCPUNum    int64  `json:"vcpu_num,omitempty"`
	VCPUMax    int64  `json:"vcpu_max,omitempty"`
	RAMMB      int64  `json:"ram_mb,omitempty"`
}

type SandboxCreateResponse struct {
	SandboxID string `json:"sandboxID"`
	Domain    string `json:"domain"`
}

// SandboxRecord is the public summary returned by GET /api/v1/sandboxes.
type SandboxRecord struct {
	TemplateID string `json:"templateID"`
	SandboxID  string `json:"sandboxID"`
	StartedAt  string `json:"startedAt"`
	CPUCount   int64  `json:"cpuCount"`
	MemoryMB   int64  `json:"memoryMB"`
}

type SandboxLifecycleRequest struct {
	SandboxID string `json:"sandbox_id"`
}

type SandboxCheckpointRequest struct {
	SandboxID string            `json:"sandbox_id"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type SandboxCheckpointResponse struct {
	Status     string `json:"status"`
	TemplateID string `json:"template_id"`
}

func (c *Client) CreateSandbox(ctx context.Context, req SandboxCreateRequest) (SandboxCreateResponse, error) {
	var resp SandboxCreateResponse
	if err := c.postJSON(ctx, createSandbox, req, &resp); err != nil {
		return SandboxCreateResponse{}, err
	}
	return resp, nil
}

func (c *Client) ListSandboxes(ctx context.Context) ([]SandboxRecord, error) {
	path := listSandbox
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("create request %s: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	var records []SandboxRecord
	if err := decodeResponse(resp, path, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func (c *Client) DeleteSandbox(ctx context.Context, sandboxID string) error {
	path := deleteSandbox + url.PathEscape(sandboxID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request %s: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()
	return decodeResponse(resp, path, nil)
}

func (c *Client) SuspendSandbox(ctx context.Context, req SandboxLifecycleRequest) error {
	return c.postJSON(ctx, suspendSandbox, req, nil)
}

func (c *Client) ResumeSandbox(ctx context.Context, req SandboxLifecycleRequest) error {
	return c.postJSON(ctx, resumeSandbox, req, nil)
}

func (c *Client) CheckpointSandbox(ctx context.Context, req SandboxCheckpointRequest) (SandboxCheckpointResponse, error) {
	var resp SandboxCheckpointResponse
	if err := c.postJSON(ctx, checkpointSandbox, req, &resp); err != nil {
		return SandboxCheckpointResponse{}, err
	}
	return resp, nil
}
