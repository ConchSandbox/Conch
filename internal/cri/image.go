package cri

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/containerd/errdefs"
	"github.com/openeuler/Conch/internal/runtimeapi"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func (s *service) PullImage(ctx context.Context, req *runtimev1.PullImageRequest) (*runtimev1.PullImageResponse, error) {
	imageName := req.GetImage().GetImage()
	auth := req.GetAuth()
	result, err := s.runtime.PullImage(ctx, runtimeapi.PullImageOptions{
		ImageName:          imageName,
		Username:           auth.GetUsername(),
		Password:           auth.GetPassword(),
		DefaultKernelImage: s.cfg.DefaultKernelImage,
	})
	if err != nil {
		return nil, err
	}
	imageRef := imageName
	images, listErr := s.runtime.ListImages(ctx, runtimeapi.ListImagesOptions{})
	if listErr == nil {
		if resolved := newImageIndex(images).resolve(imageName); resolved != nil {
			imageRef = resolved.ID
		}
	}
	if imageRef == imageName {
		if rootfsID := result.Refs["rootfs"]; rootfsID != "" {
			imageRef = rootfsID
		}
	}
	return &runtimev1.PullImageResponse{ImageRef: imageRef}, nil
}

func (s *service) ImageStatus(ctx context.Context, req *runtimev1.ImageStatusRequest) (*runtimev1.ImageStatusResponse, error) {
	images, err := s.runtime.ListImages(ctx, runtimeapi.ListImagesOptions{})
	if err != nil {
		return nil, err
	}
	view := newImageIndex(images).resolve(req.GetImage().GetImage())
	if view != nil && view.Ready {
		resp := &runtimev1.ImageStatusResponse{Image: criImage(*view)}
		if req.GetVerbose() {
			resp.Info = map[string]string{"conch": encodeInfo(verboseImageInfo(*view))}
		}
		return resp, nil
	}
	return &runtimev1.ImageStatusResponse{}, nil
}

func (s *service) ListImages(ctx context.Context, req *runtimev1.ListImagesRequest) (*runtimev1.ListImagesResponse, error) {
	images, err := s.runtime.ListImages(ctx, runtimeapi.ListImagesOptions{})
	if err != nil {
		return nil, err
	}
	filterImage := ""
	if filter := req.GetFilter(); filter != nil && filter.GetImage() != nil {
		filterImage = filter.GetImage().GetImage()
	}
	index := newImageIndex(images)
	matches := index.list(filterImage)
	out := make([]*runtimev1.Image, 0, len(matches))
	for _, view := range matches {
		if !view.Ready {
			continue
		}
		out = append(out, criImage(*view))
	}
	return &runtimev1.ListImagesResponse{Images: out}, nil
}

func (s *service) RemoveImage(ctx context.Context, req *runtimev1.RemoveImageRequest) (*runtimev1.RemoveImageResponse, error) {
	ref := req.GetImage().GetImage()
	images, err := s.runtime.ListImages(ctx, runtimeapi.ListImagesOptions{})
	if err != nil {
		return nil, err
	}
	index := newImageIndex(images)
	resolved := index.resolve(ref)
	if resolved == nil || !resolved.Ready {
		return &runtimev1.RemoveImageResponse{}, nil
	}
	if len(resolved.OriginalRefs) > 0 {
		for _, imageRef := range resolved.OriginalRefs {
			if err := s.runtime.RemoveImage(ctx, runtimeapi.RemoveImageOptions{
				ImageName:   imageRef,
				Synchronous: true,
			}); err != nil {
				if !errdefs.IsNotFound(err) {
					return nil, err
				}
			}
		}
		return &runtimev1.RemoveImageResponse{}, nil
	}
	if err := s.runtime.RemoveImage(ctx, runtimeapi.RemoveImageOptions{
		ImageName:   ref,
		Synchronous: true,
	}); err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, err
		}
	}
	return &runtimev1.RemoveImageResponse{}, nil
}

func (s *service) ImageFsInfo(context.Context, *runtimev1.ImageFsInfoRequest) (*runtimev1.ImageFsInfoResponse, error) {
	return &runtimev1.ImageFsInfoResponse{ImageFilesystems: []*runtimev1.FilesystemUsage{}}, nil
}

func encodeInfo(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func criImage(image imageView) *runtimev1.Image {
	id := image.ID
	if id == "" {
		if len(image.RepoTags) > 0 {
			id = image.RepoTags[0]
		}
	}
	size := uint64(0)
	if image.Size > 0 {
		size = uint64(image.Size)
	}
	repoTags := cloneStrings(image.RepoTags)
	repoDigests := cloneStrings(image.RepoDigests)
	sort.Strings(repoDigests)
	return &runtimev1.Image{
		Id:          id,
		RepoTags:    repoTags,
		RepoDigests: repoDigests,
		Size:        size,
		Spec: &runtimev1.ImageSpec{
			Image:       firstString(image.RepoTags, image.ID),
			Annotations: image.Labels,
		},
	}
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func firstString(values []string, fallback string) string {
	if len(values) > 0 && values[0] != "" {
		return values[0]
	}
	return fallback
}

type imageStatusInfo struct {
	ID           string            `json:"id,omitempty"`
	RepoTags     []string          `json:"repoTags,omitempty"`
	RepoDigests  []string          `json:"repoDigests,omitempty"`
	Kind         string            `json:"kind,omitempty"`
	Kinds        []string          `json:"kinds,omitempty"`
	Components   []string          `json:"components,omitempty"`
	TargetMedia  string            `json:"targetMedia,omitempty"`
	Size         int64             `json:"size,omitempty"`
	Ready        bool              `json:"ready"`
	OriginalRefs []string          `json:"originalRefs,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

func verboseImageInfo(view imageView) imageStatusInfo {
	return imageStatusInfo{
		ID:           view.ID,
		RepoTags:     cloneStrings(view.RepoTags),
		RepoDigests:  cloneStrings(view.RepoDigests),
		Kind:         view.Kind,
		Kinds:        cloneStrings(view.Kinds),
		Components:   cloneStrings(view.Components),
		TargetMedia:  view.TargetMedia,
		Size:         view.Size,
		Ready:        view.Ready,
		OriginalRefs: cloneStrings(view.OriginalRefs),
		Labels:       cloneMap(view.Labels),
	}
}
