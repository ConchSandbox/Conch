package cri

import (
	"context"
	"encoding/json"

	"github.com/openeuler/Conch/internal/runtimeapi"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func (s *service) PullImage(ctx context.Context, req *runtimev1.PullImageRequest) (*runtimev1.PullImageResponse, error) {
	imageName := req.GetImage().GetImage()
	ns := ""
	if req.GetSandboxConfig() != nil && req.GetSandboxConfig().GetMetadata() != nil {
		ns = req.GetSandboxConfig().GetMetadata().GetNamespace()
	}
	auth := req.GetAuth()
	result, err := s.runtime.PullImage(ctx, runtimeapi.PullImageOptions{
		ImageName:          imageName,
		Namespace:          ns,
		Username:           auth.GetUsername(),
		Password:           auth.GetPassword(),
		DefaultKernelImage: s.cfg.DefaultKernelImage,
	})
	if err != nil {
		return nil, err
	}
	imageRef := imageName
	if rootfsID := result.Refs["rootfs"]; rootfsID != "" {
		imageRef = rootfsID
	}
	return &runtimev1.PullImageResponse{ImageRef: imageRef}, nil
}

func (s *service) ImageStatus(context.Context, *runtimev1.ImageStatusRequest) (*runtimev1.ImageStatusResponse, error) {
	return &runtimev1.ImageStatusResponse{}, nil
}

func (s *service) ListImages(context.Context, *runtimev1.ListImagesRequest) (*runtimev1.ListImagesResponse, error) {
	return &runtimev1.ListImagesResponse{}, nil
}

func (s *service) RemoveImage(context.Context, *runtimev1.RemoveImageRequest) (*runtimev1.RemoveImageResponse, error) {
	return &runtimev1.RemoveImageResponse{}, nil
}

func (s *service) ImageFsInfo(context.Context, *runtimev1.ImageFsInfoRequest) (*runtimev1.ImageFsInfoResponse, error) {
	return &runtimev1.ImageFsInfoResponse{}, nil
}

func encodeInfo(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
