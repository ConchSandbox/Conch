package cri

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	// Full target ref (repo:tag); highest precedence.
	annotationSnapshotImage = "conch.io/snapshot-image"
	// Target repo only; tag is auto-generated. Ignored if annotationSnapshotImage is set.
	annotationSnapshotRepo = "conch.io/snapshot-repo"
	// localSnapshotRepo tags the local image when push is disabled and no target
	// repo is configured; the localhost prefix marks it as not registry-bound.
	localSnapshotRepo = "localhost/conch-snapshots"
)

// CheckpointOptions is a request to snapshot a running sandbox into an OCI image,
// optionally pushing it to a registry.
type CheckpointOptions struct {
	Namespace string
	SandboxID string
	ImageRef  string // full target ref (repo:tag)
	Push      bool   // push to registry; false = local-only (restore pinned to this node)
	PlainHTTP bool
	Username  string
	Password  string
	// ArchivePath, when set, is the kubelet-provided CheckpointContainer location:
	// the implementation must write a real checkpoint archive (OCI image tar) there.
	ArchivePath string
}

// CheckpointResult is the outcome of a successful checkpoint.
type CheckpointResult struct {
	ImageRef string
	Digest   string
}

// Checkpointer snapshots a running sandbox into an OCI image and pushes it to a
// registry so it can be restored on any node. Implemented by the daemon.
type Checkpointer interface {
	CheckpointSandbox(ctx context.Context, opts CheckpointOptions) (CheckpointResult, error)
}

// CheckpointContainer implements the CRI CheckpointContainer RPC: resolve the
// container to its PodSandbox VM and snapshot it to an OCI image.
func (s *service) CheckpointContainer(ctx context.Context, req *runtimev1.CheckpointContainerRequest) (*runtimev1.CheckpointContainerResponse, error) {
	logger := ulog.GetLogger()

	containerID := strings.TrimSpace(req.GetContainerId())
	if containerID == "" {
		return nil, status.Error(codes.InvalidArgument, "container_id is required")
	}
	if s.checkpointer == nil {
		return nil, status.Error(codes.Unimplemented, "snapshot checkpoint is not enabled on this node")
	}

	ctr, err := s.store.GetContainer(ctx, containerID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "container %s not found: %v", containerID, err)
	}
	if ctr.State != state.ContainerRunning {
		return nil, status.Errorf(codes.FailedPrecondition,
			"container %s is not running (state %s)", containerID, ctr.State)
	}
	sandboxID := strings.TrimSpace(ctr.PodSandboxID)
	if sandboxID == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s has no pod sandbox", containerID)
	}
	sbx, err := s.store.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "pod sandbox %s not found: %v", sandboxID, err)
	}
	if sbx.State != state.SandboxReady {
		return nil, status.Errorf(codes.FailedPrecondition,
			"pod sandbox %s is not ready (state %s)", sandboxID, sbx.State)
	}

	imageRef, err := resolveCheckpointImageRef(&sbx, s.cfg.Snapshot.DefaultRepo, s.cfg.Snapshot.Push)
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
		Namespace:   sbx.Namespace,
		SandboxID:   sandboxID,
		ImageRef:    imageRef,
		Push:        s.cfg.Snapshot.Push,
		PlainHTTP:   s.cfg.Snapshot.PlainHTTP,
		Username:    s.cfg.Snapshot.Username,
		Password:    s.cfg.Snapshot.Password,
		ArchivePath: strings.TrimSpace(req.GetLocation()),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checkpoint sandbox %s: %v", sandboxID, err)
	}

	logger.Info("CRI checkpoint container completed",
		ulog.F("sandbox_id", sandboxID),
		ulog.F("image_ref", res.ImageRef),
		ulog.F("digest", res.Digest),
	)

	return &runtimev1.CheckpointContainerResponse{}, nil
}

// resolveCheckpointImageRef derives the target ref from annotations or defaultRepo
// (auto-tagged); falls back to a localhost ref when push is off, else errors.
func resolveCheckpointImageRef(sbx *state.SandboxRecord, defaultRepo string, push bool) (string, error) {
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
	if !push {
		return localSnapshotRepo + "/" + snapshotImageName(sbx) + ":" + generateSnapshotTag(), nil
	}
	return "", fmt.Errorf(
		"no snapshot target: set pod annotation %q (full ref) or %q (repo), or configure cri.snapshot.default_repo",
		annotationSnapshotImage, annotationSnapshotRepo)
}

// snapshotImageName is the per-sandbox image name with the Kubernetes namespace
// (PodNamespace) folded in; sbx.Namespace (containerd ns) is only a fallback.
func snapshotImageName(sbx *state.SandboxRecord) string {
	name := strings.TrimSpace(sbx.Name)
	if name == "" {
		name = sbx.PodSandboxID
	}
	ns := strings.TrimSpace(sbx.PodNamespace)
	if ns == "" {
		ns = strings.TrimSpace(sbx.Namespace)
	}
	if ns != "" {
		return ns + "/" + name
	}
	return name
}

// generateSnapshotTag returns a timestamp plus a random suffix, so concurrent or
// sub-second checkpoints of the same pod don't collide and overwrite each other.
func generateSnapshotTag() string {
	return time.Now().UTC().Format("20060102-150405") + "-" + randomTagSuffix()
}

func randomTagSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%09d", time.Now().UTC().Nanosecond())
	}
	return hex.EncodeToString(b[:])
}
