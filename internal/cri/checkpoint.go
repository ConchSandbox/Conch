package cri

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	// annotationSnapshotImage, when set on the sandbox (Pod), is the full target
	// image reference (repo:tag) the checkpoint snapshot is pushed to. Highest
	// precedence.
	annotationSnapshotImage = "conch.io/snapshot-image"
	// annotationSnapshotRepo, when set, is the target repository; the tag is
	// generated automatically. Ignored when annotationSnapshotImage is set.
	annotationSnapshotRepo = "conch.io/snapshot-repo"
)

// CheckpointOptions describes a request to checkpoint a running sandbox into a
// snapshot OCI image and push it to a registry.
type CheckpointOptions struct {
	Namespace string
	SandboxID string
	// ImageRef is the full target reference (repo:tag) to push the snapshot to.
	ImageRef  string
	PlainHTTP bool
	Username  string
	Password  string
}

// CheckpointResult is the outcome of a successful checkpoint.
type CheckpointResult struct {
	ImageRef string
	Digest   string
}

// Checkpointer captures a running sandbox into a snapshot OCI image and pushes
// it to a registry so it can be restored on any node. It is implemented by the
// daemon, reusing the existing snapshot export + image push paths.
type Checkpointer interface {
	CheckpointSandbox(ctx context.Context, opts CheckpointOptions) (CheckpointResult, error)
}

// CheckpointContainer implements the CRI CheckpointContainer RPC.
//
// kubelet routes this call to the node currently running the Pod, so there is
// no need for a central controller to locate the sandbox. In Conch a CRI
// container is a metadata record; the real workload is the PodSandbox VM, so we
// resolve the container to its PodSandbox and snapshot that VM. The snapshot is
// exported as an OCI image and pushed to a registry (the cross-node shared
// layer); restore happens elsewhere via the existing conch.io/use-snapshot path.
//
// Note: the kubelet checkpoint API carries no field to name the snapshot or
// pick a registry, so the target image reference is derived from the Pod
// annotations (conch.io/snapshot-image / conch.io/snapshot-repo) or the
// node's cri.snapshot.default_repo config.
func (s *service) CheckpointContainer(ctx context.Context, req *runtimev1.CheckpointContainerRequest) (*runtimev1.CheckpointContainerResponse, error) {
	logger := ulog.GetLogger()

	containerID := strings.TrimSpace(req.GetContainerId())
	if containerID == "" {
		return nil, status.Error(codes.InvalidArgument, "container_id is required")
	}
	if s.checkpointer == nil {
		return nil, status.Error(codes.Unimplemented, "snapshot checkpoint is not enabled on this node")
	}

	// Resolve container -> pod sandbox (VM).
	ctr, err := s.store.GetContainer(ctx, containerID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "container %s not found: %v", containerID, err)
	}
	sandboxID := strings.TrimSpace(ctr.PodSandboxID)
	if sandboxID == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s has no pod sandbox", containerID)
	}
	sbx, err := s.store.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "pod sandbox %s not found: %v", sandboxID, err)
	}

	imageRef, err := resolveCheckpointImageRef(&sbx, s.cfg.Snapshot.DefaultRepo)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if req.GetTimeout() > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.GetTimeout())*time.Second)
		defer cancel()
	}

	logger.Info("CRI checkpoint container",
		ulog.F("container_id", containerID),
		ulog.F("sandbox_id", sandboxID),
		ulog.F("image_ref", imageRef),
	)

	res, err := s.checkpointer.CheckpointSandbox(ctx, CheckpointOptions{
		Namespace: sbx.Namespace,
		SandboxID: sandboxID,
		ImageRef:  imageRef,
		PlainHTTP: s.cfg.Snapshot.PlainHTTP,
		Username:  s.cfg.Snapshot.Username,
		Password:  s.cfg.Snapshot.Password,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checkpoint sandbox %s: %v", sandboxID, err)
	}

	// Write a small pointer manifest to the kubelet-provided Location so the
	// checkpoint API has a non-empty artifact and callers can discover the
	// pushed image reference. The durable artifact lives in the registry, so a
	// failure here is non-fatal.
	if loc := strings.TrimSpace(req.GetLocation()); loc != "" {
		if err := writeCheckpointLocation(loc, res); err != nil {
			logger.Warn("failed to write checkpoint location pointer",
				ulog.F("location", loc), ulog.F("error", err))
		}
	}

	logger.Info("CRI checkpoint container completed",
		ulog.F("sandbox_id", sandboxID),
		ulog.F("image_ref", res.ImageRef),
		ulog.F("digest", res.Digest),
	)

	return &runtimev1.CheckpointContainerResponse{}, nil
}

// resolveCheckpointImageRef derives the target push reference from the sandbox
// annotations, falling back to the node default repo. The tag is auto-generated
// when only a repository is supplied.
func resolveCheckpointImageRef(sbx *state.SandboxRecord, defaultRepo string) (string, error) {
	if sbx.Annotations != nil {
		if ref := strings.TrimSpace(sbx.Annotations[annotationSnapshotImage]); ref != "" {
			return ref, nil
		}
		if repo := strings.TrimSpace(sbx.Annotations[annotationSnapshotRepo]); repo != "" {
			return strings.TrimRight(repo, "/") + ":" + generateSnapshotTag(), nil
		}
	}
	if repo := strings.TrimSpace(defaultRepo); repo != "" {
		return strings.TrimRight(repo, "/") + "/" + snapshotImageName(sbx) + ":" + generateSnapshotTag(), nil
	}
	return "", fmt.Errorf(
		"no snapshot target: set pod annotation %q (full ref) or %q (repo), or configure cri.snapshot.default_repo",
		annotationSnapshotImage, annotationSnapshotRepo)
}

func snapshotImageName(sbx *state.SandboxRecord) string {
	if n := strings.TrimSpace(sbx.Name); n != "" {
		return n
	}
	return sbx.PodSandboxID
}

func generateSnapshotTag() string {
	return time.Now().UTC().Format("20060102-150405")
}

func writeCheckpointLocation(location string, res CheckpointResult) error {
	if err := os.MkdirAll(filepath.Dir(location), 0o755); err != nil {
		return fmt.Errorf("create location dir: %w", err)
	}
	payload, err := json.Marshal(map[string]string{
		"image_ref": res.ImageRef,
		"digest":    res.Digest,
		"created":   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal checkpoint location: %w", err)
	}
	if err := os.WriteFile(location, payload, 0o644); err != nil {
		return fmt.Errorf("write checkpoint location: %w", err)
	}
	return nil
}
