package client

import (
	"context"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

const (
	pullImage   = "/api/image/pull"
	pushImage   = "/api/image/push"
	listImages  = "/api/image/list"
	removeImage = "/api/image/remove"
	unpackImage = "/api/image/unpack"
)

type PullImageRequest struct {
	ImageName  string `json:"image_name"`
	PlainHTTP  bool   `json:"plain_http,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	SkipUnpack bool   `json:"skip_unpack,omitempty"`
}

type PushImageRequest struct {
	LocalImage  string `json:"local_image"`
	RemoteImage string `json:"remote_image"`
	PlainHTTP   bool   `json:"plain_http,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
}

type UnpackImageRequest struct {
	ImageName string `json:"image_name"`
}

type ListImagesRequest struct {
	Filters []string `json:"filters,omitempty"`
}

type RemoveImageRequest struct {
	ImageName   string `json:"image_name"`
	Synchronous bool   `json:"synchronous,omitempty"`
}

type imageResponse struct {
	Results map[string]string `json:"results"`
}

type ImageRecord = runtimeapi.ImageRecord

type listImagesResponse struct {
	Images []ImageRecord `json:"images"`
}

func (c *Client) PullImage(ctx context.Context, req PullImageRequest) (map[string]string, error) {
	var resp imageResponse
	if err := c.postJSON(ctx, pullImage, req, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

func (c *Client) PushImage(ctx context.Context, req PushImageRequest) error {
	return c.postJSON(ctx, pushImage, req, nil)
}

func (c *Client) ListImages(ctx context.Context, req ListImagesRequest) ([]ImageRecord, error) {
	var resp listImagesResponse
	if err := c.postJSON(ctx, listImages, req, &resp); err != nil {
		return nil, err
	}
	return resp.Images, nil
}

func (c *Client) RemoveImage(ctx context.Context, req RemoveImageRequest) error {
	return c.postJSON(ctx, removeImage, req, nil)
}

func (c *Client) UnpackImage(ctx context.Context, req UnpackImageRequest) (map[string]string, error) {
	var resp imageResponse
	if err := c.postJSON(ctx, unpackImage, req, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}
