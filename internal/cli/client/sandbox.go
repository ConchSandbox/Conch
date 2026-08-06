package client

import "context"

const (
	createSandbox     = "/api/v1/sandboxes"
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
