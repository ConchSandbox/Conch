package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	imageSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/image"
	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/pkg/ulog"
)

// snapshotCheckpointRequest is the input for POST /api/snapshot/checkpoint: it
// snapshots a running sandbox VM into an OCI image and pushes it to a registry so
// the snapshot can be restored on any node. The push target (ImageRef) is supplied
// by the caller, so the control plane knows the exact ref/digest with no side
// channel. Used by conch-committer (the OpenSandbox image-committer drop-in).
type snapshotCheckpointRequest struct {
	Namespace string `json:"namespace,omitempty"`
	// Exactly one of SandboxID / PodName targets the VM. PodName is the k8s Pod
	// name (with Namespace its k8s namespace); when SandboxID is empty conchd
	// resolves the CRI sandbox id from the state store (resolveSandboxByPod).
	SandboxID string `json:"sandbox_id,omitempty"`
	PodName   string `json:"pod_name,omitempty"`
	ImageRef  string `json:"image_ref"`
	PlainHTTP bool   `json:"plain_http,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
}

type snapshotCheckpointResponse struct {
	ImageRef    string `json:"image_ref"`
	ImageDigest string `json:"image_digest"`
}

// handleSnapshotCheckpoint commits a running sandbox VM into an OCI snapshot image
// and pushes it to a registry, composing the existing snapshot export (pause +
// commit) with the existing registry push.
func (s *Daemon) handleSnapshotCheckpoint(w http.ResponseWriter, r *http.Request) {
	logger := ulog.GetLogger()
	logger.Debug("Handling snapshot checkpoint request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeService == nil || s.runtimeService.Image == nil ||
		s.runtimeService.Snapshot == nil || s.runtimeService.Sandbox == nil {
		http.Error(w, "Image, snapshot or sandbox service unavailable", http.StatusServiceUnavailable)
		return
	}

	var req snapshotCheckpointRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("Invalid request body", ulog.F("error", err))
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ImageRef) == "" {
		http.Error(w, "image_ref is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" && strings.TrimSpace(req.PodName) == "" {
		http.Error(w, "either sandbox_id or pod_name is required", http.StatusBadRequest)
		return
	}

	// Callers may only know the k8s Pod identity; resolve it to the CRI sandbox id.
	if strings.TrimSpace(req.SandboxID) == "" {
		rec, err := s.resolveSandboxByPod(r.Context(), strings.TrimSpace(req.Namespace), strings.TrimSpace(req.PodName))
		if err != nil {
			logger.Warn("Failed to resolve sandbox from pod",
				ulog.F("pod_name", req.PodName),
				ulog.F("namespace", req.Namespace),
				ulog.F("error", err),
			)
			http.Error(w, "Failed to resolve sandbox from pod: "+err.Error(), http.StatusNotFound)
			return
		}
		req.SandboxID = rec.PodSandboxID
		req.Namespace = rec.Namespace // record's containerd namespace, not the k8s one
		logger.Info("Resolved sandbox from pod",
			ulog.F("pod_name", req.PodName),
			ulog.F("sandbox_id", req.SandboxID),
		)
	}

	resp, err := s.checkpointSandbox(r.Context(), req)
	if err != nil {
		logger.Error("Failed to checkpoint sandbox",
			ulog.F("sandbox_id", req.SandboxID),
			ulog.F("image_ref", req.ImageRef),
			ulog.F("error", err),
		)
		writeImageError(w, "Failed to checkpoint sandbox", err)
		return
	}

	logger.Info("Sandbox checkpoint completed",
		ulog.F("sandbox_id", req.SandboxID),
		ulog.F("image_ref", resp.ImageRef),
		ulog.F("image_digest", resp.ImageDigest),
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// checkpointSandbox pauses + commits a running sandbox into a local OCI image and
// pushes it to the registry referenced by req.ImageRef.
func (s *Daemon) checkpointSandbox(ctx context.Context, req snapshotCheckpointRequest) (snapshotCheckpointResponse, error) {
	exportResp, err := s.exportSnapshotImage(ctx, snapshotExportRequest{
		Namespace:    req.Namespace,
		SandboxID:    req.SandboxID,
		BootIndexTag: req.ImageRef,
	})
	if err != nil {
		return snapshotCheckpointResponse{}, fmt.Errorf("export snapshot image: %w", err)
	}

	if err := s.runtimeService.PushImageRequest(ctx, imageSvc.PushRequest{
		LocalImage:  req.ImageRef,
		RemoteImage: req.ImageRef,
		Namespace:   s.resolveNamespace(req.Namespace),
		PlainHTTP:   req.PlainHTTP,
		Username:    req.Username,
		Password:    req.Password,
	}); err != nil {
		return snapshotCheckpointResponse{}, fmt.Errorf("push snapshot image %s: %w", req.ImageRef, err)
	}

	return snapshotCheckpointResponse{
		ImageRef:    exportResp.BootIndexTag,
		ImageDigest: exportResp.BootIndexDigest,
	}, nil
}

// resolveSandboxByPod maps a Kubernetes (namespace, pod name) to the CRI sandbox
// record conchd tracks the VM by (recorded at RunPodSandbox time). An empty
// podNamespace matches any namespace; the match must be unique.
func (s *Daemon) resolveSandboxByPod(ctx context.Context, podNamespace, podName string) (state.SandboxRecord, error) {
	if strings.TrimSpace(podName) == "" {
		return state.SandboxRecord{}, fmt.Errorf("pod_name is required")
	}
	if s.stateStore == nil {
		return state.SandboxRecord{}, fmt.Errorf("state store unavailable")
	}

	records, err := s.stateStore.ListSandboxes(ctx)
	if err != nil {
		return state.SandboxRecord{}, fmt.Errorf("list sandboxes: %w", err)
	}

	var matches []state.SandboxRecord
	for _, rec := range records {
		if rec.Name != podName {
			continue
		}
		if podNamespace != "" && rec.PodNamespace != podNamespace {
			continue
		}
		matches = append(matches, rec)
	}

	switch len(matches) {
	case 0:
		return state.SandboxRecord{}, fmt.Errorf("no sandbox found for pod %q in namespace %q", podName, podNamespace)
	case 1:
		return matches[0], nil
	default:
		return state.SandboxRecord{}, fmt.Errorf("ambiguous: %d sandboxes match pod %q in namespace %q", len(matches), podName, podNamespace)
	}
}
