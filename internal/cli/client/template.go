package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

const (
	createTemplate  = "/api/template/create"
	pullTemplate    = "/api/template/pull"
	pushTemplate    = "/api/template/push"
	listTemplates   = "/api/template/list"
	inspectTemplate = "/api/template/inspect"
	removeTemplate  = "/api/template/remove"
)

type TemplateIDRequest struct {
	ID string `json:"id"`
}

type TemplateListRequest struct {
	Origin   string `json:"origin,omitempty"`
	BootMode string `json:"boot_mode,omitempty"`
}

type TemplateRecord = runtimeapi.TemplateRecord

type templateListResponse struct {
	Items []TemplateRecord `json:"items"`
}

type TemplatePullRequest struct {
	Reference string            `json:"reference"`
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
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
}

type TemplateCreateRequest struct {
	Source       string
	KernelPath   string
	InitrdPath   string
	BootIndexTag string
	PlainHTTP    bool
	Username     string
	Password     string
	Labels       map[string]string
}

type TemplateCreateMetadata struct {
	Source       string            `json:"source"`
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

func (c *Client) ListTemplates(ctx context.Context, req TemplateListRequest) ([]TemplateRecord, error) {
	var resp templateListResponse
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
	return c.postJSON(ctx, removeTemplate, TemplateIDRequest{ID: id}, nil)
}

func (c *Client) PullTemplate(ctx context.Context, req TemplatePullRequest) (TemplatePullResponse, error) {
	var resp TemplatePullResponse
	if err := c.postJSON(ctx, pullTemplate, req, &resp); err != nil {
		return TemplatePullResponse{}, err
	}
	return resp, nil
}

func (c *Client) PushTemplate(ctx context.Context, req TemplatePushRequest) error {
	return c.postJSON(ctx, pushTemplate, req, nil)
}

func (c *Client) CreateTemplate(ctx context.Context, req TemplateCreateRequest) (TemplateCreateResponse, error) {
	metadata, err := json.Marshal(TemplateCreateMetadata{
		Source:       req.Source,
		BootIndexTag: req.BootIndexTag,
		PlainHTTP:    req.PlainHTTP,
		Username:     req.Username,
		Password:     req.Password,
		Labels:       req.Labels,
	})
	if err != nil {
		return TemplateCreateResponse{}, fmt.Errorf("marshal template create metadata: %w", err)
	}
	var resp TemplateCreateResponse
	if err := c.postTemplateMultipart(ctx, req.KernelPath, req.InitrdPath, metadata, &resp); err != nil {
		return TemplateCreateResponse{}, err
	}
	return resp, nil
}

func (c *Client) postTemplateMultipart(ctx context.Context, kernelPath, initrdPath string, metadata []byte, out any) error {
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

	reader, writerPipe := io.Pipe()
	writer := multipart.NewWriter(writerPipe)
	go func() {
		writeErr := writeTemplateMultipart(writer, kernel, initrd, kernelPath, initrdPath, metadata)
		if writeErr != nil {
			_ = writerPipe.CloseWithError(writeErr)
			return
		}
		_ = writerPipe.Close()
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+createTemplate, reader)
	if err != nil {
		_ = reader.Close()
		return fmt.Errorf("create request %s: %w", createTemplate, err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST %s: %w", createTemplate, err)
	}
	defer resp.Body.Close()
	return decodeResponse(resp, createTemplate, out)
}

func writeTemplateMultipart(writer *multipart.Writer, kernel, initrd io.Reader, kernelPath, initrdPath string, metadata []byte) error {
	if err := writer.WriteField("metadata", string(metadata)); err != nil {
		return err
	}
	kernelPart, err := writer.CreateFormFile("kernel", filepath.Base(kernelPath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(kernelPart, kernel); err != nil {
		return err
	}
	initrdPart, err := writer.CreateFormFile("initrd", filepath.Base(initrdPath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(initrdPart, initrd); err != nil {
		return err
	}
	return writer.Close()
}
