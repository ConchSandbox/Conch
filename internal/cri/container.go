package cri

import (
	"context"
	"encoding/json"
	"time"

	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

func (s *service) CreateContainer(ctx context.Context, req *runtimev1.CreateContainerRequest) (*runtimev1.CreateContainerResponse, error) {
	cfg := req.GetConfig()
	meta := cfg.GetMetadata()
	image := cfg.GetImage()
	imageSpec := image.GetImage()
	res, err := s.runtime.CreateContainer(ctx, runtimeapi.ContainerCreateOptions{
		PodSandboxID: req.GetPodSandboxId(),
		Name:         meta.GetName(),
		Image:        imageSpec,
		ImageRef:     imageSpec,
		Command:      cfg.GetCommand(),
		Args:         cfg.GetArgs(),
		LogPath:      cfg.GetLogPath(),
		Labels:       cfg.GetLabels(),
		Annotations:  cfg.GetAnnotations(),
	})
	if err != nil {
		return nil, err
	}
	return &runtimev1.CreateContainerResponse{ContainerId: res.ContainerID}, nil
}

func (s *service) StartContainer(ctx context.Context, req *runtimev1.StartContainerRequest) (*runtimev1.StartContainerResponse, error) {
	if err := s.runtime.SetContainerState(ctx, req.GetContainerId(), state.ContainerRunning); err != nil {
		return nil, err
	}
	return &runtimev1.StartContainerResponse{}, nil
}

func (s *service) StopContainer(ctx context.Context, req *runtimev1.StopContainerRequest) (*runtimev1.StopContainerResponse, error) {
	if err := s.runtime.SetContainerState(ctx, req.GetContainerId(), state.ContainerExited); err != nil {
		return nil, err
	}
	return &runtimev1.StopContainerResponse{}, nil
}

func (s *service) RemoveContainer(ctx context.Context, req *runtimev1.RemoveContainerRequest) (*runtimev1.RemoveContainerResponse, error) {
	if err := s.store.DeleteContainer(ctx, req.GetContainerId()); err != nil {
		return nil, err
	}
	return &runtimev1.RemoveContainerResponse{}, nil
}

func (s *service) ListContainers(ctx context.Context, req *runtimev1.ListContainersRequest) (*runtimev1.ListContainersResponse, error) {
	records, err := s.store.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	var containers []*runtimev1.Container
	for _, rec := range records {
		if !matchContainerFilter(rec, req.GetFilter()) {
			continue
		}
		imageRef := containerImageRef(rec)
		containers = append(containers, &runtimev1.Container{
			Id:           rec.ContainerID,
			PodSandboxId: rec.PodSandboxID,
			Metadata:     &runtimev1.ContainerMetadata{Name: rec.Name},
			Image:        &runtimev1.ImageSpec{Image: rec.Image},
			ImageRef:     imageRef,
			State:        containerState(rec.State),
			CreatedAt:    rec.CreatedAt,
			Labels:       rec.Labels,
			Annotations:  rec.Annotations,
			ImageId:      imageRef,
		})
	}
	return &runtimev1.ListContainersResponse{Containers: containers}, nil
}

func (s *service) ContainerStatus(ctx context.Context, req *runtimev1.ContainerStatusRequest) (*runtimev1.ContainerStatusResponse, error) {
	rec, err := s.store.GetContainer(ctx, req.GetContainerId())
	if err != nil {
		return nil, err
	}
	imageRef := containerImageRef(rec)
	resp := &runtimev1.ContainerStatusResponse{
		Status: &runtimev1.ContainerStatus{
			Id:          rec.ContainerID,
			Metadata:    &runtimev1.ContainerMetadata{Name: rec.Name},
			State:       containerState(rec.State),
			CreatedAt:   rec.CreatedAt,
			StartedAt:   rec.StartedAt,
			FinishedAt:  rec.FinishedAt,
			ExitCode:    rec.ExitCode,
			Image:       &runtimev1.ImageSpec{Image: rec.Image},
			ImageRef:    imageRef,
			Reason:      rec.State,
			Message:     rec.LastError,
			Labels:      rec.Labels,
			Annotations: rec.Annotations,
			LogPath:     rec.LogPath,
			ImageId:     imageRef,
		},
	}
	if req.GetVerbose() {
		payload, _ := json.Marshal(rec)
		resp.Info = map[string]string{"conch": string(payload)}
	}
	return resp, nil
}

func containerImageRef(rec state.ContainerRecord) string {
	if rec.ImageRef != "" {
		return rec.ImageRef
	}
	return rec.Image
}

func containerState(value string) runtimev1.ContainerState {
	switch value {
	case state.ContainerCreated:
		return runtimev1.ContainerState_CONTAINER_CREATED
	case state.ContainerRunning:
		return runtimev1.ContainerState_CONTAINER_RUNNING
	case state.ContainerExited:
		return runtimev1.ContainerState_CONTAINER_EXITED
	case state.ContainerUnknown:
		return runtimev1.ContainerState_CONTAINER_UNKNOWN
	default:
		return runtimev1.ContainerState_CONTAINER_UNKNOWN
	}
}

func matchContainerFilter(rec state.ContainerRecord, filter *runtimev1.ContainerFilter) bool {
	if filter == nil {
		return true
	}
	if filter.GetId() != "" && filter.GetId() != rec.ContainerID {
		return false
	}
	if filter.GetPodSandboxId() != "" && filter.GetPodSandboxId() != rec.PodSandboxID {
		return false
	}
	if filter.GetState() != nil && containerState(rec.State) != filter.GetState().GetState() {
		return false
	}
	for k, v := range filter.GetLabelSelector() {
		if rec.Labels[k] != v {
			return false
		}
	}
	return true
}

func nowNano() int64 {
	return time.Now().UnixNano()
}
