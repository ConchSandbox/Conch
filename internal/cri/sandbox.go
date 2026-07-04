package cri

import (
	"context"
	"encoding/json"
	"strconv"

	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

const (
	annotationSandboxImage = "conch.io/sandbox-image"
	annotationUseSnapshot  = "conch.io/use-snapshot"
	annotationVMMName      = "conch.io/vmm-name"
	annotationVCPU         = "conch.io/vcpu"
	annotationVCPUMax      = "conch.io/vcpu-max"
	annotationRAMMB        = "conch.io/ram-mb"
)

func (s *service) RunPodSandbox(ctx context.Context, req *runtimev1.RunPodSandboxRequest) (*runtimev1.RunPodSandboxResponse, error) {
	cfg := req.GetConfig()
	meta := cfg.GetMetadata()
	annotations := cfg.GetAnnotations()

	imageName := annotations[annotationSandboxImage]
	useSnapshot := parseBool(annotations[annotationUseSnapshot])
	vmmName := annotations[annotationVMMName]
	vcpu := parseInt64(annotations[annotationVCPU], 0)
	vcpuMax := parseInt64(annotations[annotationVCPUMax], 0)
	ramMB := parseInt64(annotations[annotationRAMMB], 0)

	res, err := s.runtime.CreateSandbox(ctx, runtimeapi.SandboxCreateOptions{
		PodNamespace:   meta.GetNamespace(),
		Name:           meta.GetName(),
		UID:            meta.GetUid(),
		Attempt:        meta.GetAttempt(),
		Labels:         cfg.GetLabels(),
		Annotations:    annotations,
		RuntimeHandler: req.GetRuntimeHandler(),
		ImageName:      imageName,
		UseSnapshot:    useSnapshot,
		VMMName:        vmmName,
		VCPUNum:        vcpu,
		VCPUMax:        vcpuMax,
		RamMB:          ramMB,
	})
	if err != nil {
		return nil, err
	}
	return &runtimev1.RunPodSandboxResponse{PodSandboxId: res.PodSandboxID}, nil
}

func (s *service) StopPodSandbox(ctx context.Context, req *runtimev1.StopPodSandboxRequest) (*runtimev1.StopPodSandboxResponse, error) {
	rec, _ := s.store.GetSandbox(ctx, req.GetPodSandboxId())
	if err := s.runtime.StopSandbox(ctx, rec.Namespace, req.GetPodSandboxId()); err != nil {
		return nil, err
	}
	return &runtimev1.StopPodSandboxResponse{}, nil
}

func (s *service) RemovePodSandbox(ctx context.Context, req *runtimev1.RemovePodSandboxRequest) (*runtimev1.RemovePodSandboxResponse, error) {
	rec, _ := s.store.GetSandbox(ctx, req.GetPodSandboxId())
	if err := s.runtime.RemoveSandbox(ctx, rec.Namespace, req.GetPodSandboxId()); err != nil {
		return nil, err
	}
	return &runtimev1.RemovePodSandboxResponse{}, nil
}

func (s *service) PodSandboxStatus(ctx context.Context, req *runtimev1.PodSandboxStatusRequest) (*runtimev1.PodSandboxStatusResponse, error) {
	rec, err := s.store.GetSandbox(ctx, req.GetPodSandboxId())
	if err != nil {
		return nil, err
	}
	resp := &runtimev1.PodSandboxStatusResponse{
		Status:    sandboxStatus(rec),
		Timestamp: nowNano(),
	}
	if req.GetVerbose() {
		payload, _ := json.Marshal(rec)
		resp.Info = map[string]string{"conch": string(payload)}
	}
	return resp, nil
}

func (s *service) ListPodSandbox(ctx context.Context, req *runtimev1.ListPodSandboxRequest) (*runtimev1.ListPodSandboxResponse, error) {
	records, err := s.store.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	var items []*runtimev1.PodSandbox
	for _, rec := range records {
		if !matchSandboxFilter(rec, req.GetFilter()) {
			continue
		}
		items = append(items, &runtimev1.PodSandbox{
			Id:             rec.PodSandboxID,
			Metadata:       sandboxMetadata(rec),
			State:          sandboxState(rec.State),
			CreatedAt:      rec.CreatedAt,
			Labels:         rec.Labels,
			Annotations:    rec.Annotations,
			RuntimeHandler: rec.RuntimeHandler,
		})
	}
	return &runtimev1.ListPodSandboxResponse{Items: items}, nil
}

func sandboxStatus(rec state.SandboxRecord) *runtimev1.PodSandboxStatus {
	return &runtimev1.PodSandboxStatus{
		Id:        rec.PodSandboxID,
		Metadata:  sandboxMetadata(rec),
		State:     sandboxState(rec.State),
		CreatedAt: rec.CreatedAt,
		Network: &runtimev1.PodSandboxNetworkStatus{
			Ip: rec.IP,
		},
		Labels:         rec.Labels,
		Annotations:    rec.Annotations,
		RuntimeHandler: rec.RuntimeHandler,
	}
}

func sandboxMetadata(rec state.SandboxRecord) *runtimev1.PodSandboxMetadata {
	namespace := rec.PodNamespace
	if namespace == "" {
		namespace = rec.Namespace
	}
	return &runtimev1.PodSandboxMetadata{
		Name:      rec.Name,
		Uid:       rec.UID,
		Namespace: namespace,
		Attempt:   rec.Attempt,
	}
}

func sandboxState(value string) runtimev1.PodSandboxState {
	if value == state.SandboxReady {
		return runtimev1.PodSandboxState_SANDBOX_READY
	}
	return runtimev1.PodSandboxState_SANDBOX_NOTREADY
}

func matchSandboxFilter(rec state.SandboxRecord, filter *runtimev1.PodSandboxFilter) bool {
	if filter == nil {
		return true
	}
	if filter.GetId() != "" && filter.GetId() != rec.PodSandboxID {
		return false
	}
	if filter.GetState() != nil && sandboxState(rec.State) != filter.GetState().GetState() {
		return false
	}
	for k, v := range filter.GetLabelSelector() {
		if rec.Labels[k] != v {
			return false
		}
	}
	return true
}

func parseBool(value string) bool {
	ok, _ := strconv.ParseBool(value)
	return ok
}

func parseInt64(value string, fallback int64) int64 {
	if value == "" {
		return fallback
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n == 0 {
		return fallback
	}
	return n
}
