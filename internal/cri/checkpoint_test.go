package cri

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/openeuler/Conch/internal/daemon/state"
)

type fakeCheckpointer struct {
	called  bool
	gotOpts CheckpointOptions
	result  CheckpointResult
	err     error
}

func (f *fakeCheckpointer) CheckpointSandbox(_ context.Context, opts CheckpointOptions) (CheckpointResult, error) {
	f.called = true
	f.gotOpts = opts
	if f.err != nil {
		return CheckpointResult{}, f.err
	}
	if f.result.ImageRef == "" {
		f.result.ImageRef = opts.ImageRef
	}
	return f.result, nil
}

func seedContainerAndSandbox(t *testing.T, store state.Store, sbx state.SandboxRecord) {
	t.Helper()
	// Default to a live workload (ready sandbox) unless the test seeds a state.
	if sbx.State == "" {
		sbx.State = state.SandboxReady
	}
	ctx := context.Background()
	if err := store.UpsertSandbox(ctx, sbx); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	if err := store.UpsertContainer(ctx, state.ContainerRecord{
		ContainerID:  "container-1",
		PodSandboxID: sbx.PodSandboxID,
		Name:         "container-a",
		State:        state.ContainerRunning,
	}); err != nil {
		t.Fatalf("UpsertContainer() error = %v", err)
	}
}

func TestCheckpointContainerUsesSnapshotImageAnnotation(t *testing.T) {
	store := newCRIStateStore(t)
	seedContainerAndSandbox(t, store, state.SandboxRecord{
		PodSandboxID: "sandbox-1",
		Namespace:    "default",
		PodNamespace: "team-a",
		Name:         "my-pod",
		State:        state.SandboxReady,
		Annotations:  map[string]string{annotationSnapshotImage: "reg.example/snaps/my-pod:v1"},
	})
	cp := &fakeCheckpointer{result: CheckpointResult{Digest: "sha256:abc"}}
	svc := &service{store: store, checkpointer: cp}

	loc := filepath.Join(t.TempDir(), "checkpoints", "ckpt.json")
	_, err := svc.CheckpointContainer(context.Background(), &runtimev1.CheckpointContainerRequest{
		ContainerId: "container-1",
		Location:    loc,
	})
	if err != nil {
		t.Fatalf("CheckpointContainer() error = %v", err)
	}
	if !cp.called {
		t.Fatal("checkpointer was not called")
	}
	if cp.gotOpts.ImageRef != "reg.example/snaps/my-pod:v1" {
		t.Fatalf("ImageRef = %q, want annotation value", cp.gotOpts.ImageRef)
	}
	// gotOpts.Namespace is the containerd namespace (export/push), not the pod ns.
	if cp.gotOpts.SandboxID != "sandbox-1" || cp.gotOpts.Namespace != "default" {
		t.Fatalf("opts = %+v, want sandbox-1/default", cp.gotOpts)
	}

	if cp.gotOpts.ArchivePath != loc {
		t.Fatalf("ArchivePath = %q, want %q", cp.gotOpts.ArchivePath, loc)
	}
}

func TestCheckpointContainerRepoAnnotationGeneratesTag(t *testing.T) {
	store := newCRIStateStore(t)
	seedContainerAndSandbox(t, store, state.SandboxRecord{
		PodSandboxID: "sandbox-1",
		Namespace:    "default",
		PodNamespace: "team-a",
		Name:         "my-pod",
		Annotations:  map[string]string{annotationSnapshotRepo: "reg.example/snaps/my-pod"},
	})
	cp := &fakeCheckpointer{}
	svc := &service{store: store, checkpointer: cp}

	if _, err := svc.CheckpointContainer(context.Background(), &runtimev1.CheckpointContainerRequest{
		ContainerId: "container-1",
	}); err != nil {
		t.Fatalf("CheckpointContainer() error = %v", err)
	}
	const wantPrefix = "reg.example/snaps/my-pod:"
	if got := cp.gotOpts.ImageRef; len(got) <= len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("ImageRef = %q, want prefix %q + generated tag", got, wantPrefix)
	}
}

func TestCheckpointContainerDefaultRepoFallback(t *testing.T) {
	store := newCRIStateStore(t)
	seedContainerAndSandbox(t, store, state.SandboxRecord{
		PodSandboxID: "sandbox-1",
		Namespace:    "default",
		PodNamespace: "team-a",
		Name:         "my-pod",
	})
	cp := &fakeCheckpointer{}
	svc := &service{store: store, checkpointer: cp, cfg: Config{Snapshot: SnapshotConfig{DefaultRepo: "reg.example/snaps"}}}

	if _, err := svc.CheckpointContainer(context.Background(), &runtimev1.CheckpointContainerRequest{
		ContainerId: "container-1",
	}); err != nil {
		t.Fatalf("CheckpointContainer() error = %v", err)
	}
	const wantPrefix = "reg.example/snaps/team-a/my-pod:"
	if got := cp.gotOpts.ImageRef; len(got) <= len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("ImageRef = %q, want default-repo derived ref", got)
	}
}

func TestCheckpointContainerNoTargetWithPushIsInvalidArgument(t *testing.T) {
	store := newCRIStateStore(t)
	seedContainerAndSandbox(t, store, state.SandboxRecord{
		PodSandboxID: "sandbox-1",
		Namespace:    "default",
		PodNamespace: "team-a",
		Name:         "my-pod",
	})
	svc := &service{store: store, checkpointer: &fakeCheckpointer{}, cfg: Config{Snapshot: SnapshotConfig{Push: true}}}

	_, err := svc.CheckpointContainer(context.Background(), &runtimev1.CheckpointContainerRequest{ContainerId: "container-1"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestCheckpointContainerLocalModeNoTargetUsesLocalRef(t *testing.T) {
	store := newCRIStateStore(t)
	seedContainerAndSandbox(t, store, state.SandboxRecord{
		PodSandboxID: "sandbox-1",
		Namespace:    "default",
		PodNamespace: "team-a",
		Name:         "my-pod",
	})
	cp := &fakeCheckpointer{}
	svc := &service{store: store, checkpointer: cp} // cfg zero value: Snapshot.Push == false

	if _, err := svc.CheckpointContainer(context.Background(), &runtimev1.CheckpointContainerRequest{ContainerId: "container-1"}); err != nil {
		t.Fatalf("CheckpointContainer() error = %v", err)
	}
	const wantPrefix = localSnapshotRepo + "/team-a/my-pod:"
	if got := cp.gotOpts.ImageRef; len(got) <= len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("ImageRef = %q, want local-only prefix %q", got, wantPrefix)
	}
	if cp.gotOpts.Push {
		t.Fatalf("opts.Push = true, want false in local mode")
	}
}

func TestCheckpointContainerNilCheckpointerUnimplemented(t *testing.T) {
	store := newCRIStateStore(t)
	svc := &service{store: store}
	_, err := svc.CheckpointContainer(context.Background(), &runtimev1.CheckpointContainerRequest{ContainerId: "container-1"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("error code = %v, want Unimplemented", status.Code(err))
	}
}

func TestCheckpointContainerUnknownContainerNotFound(t *testing.T) {
	store := newCRIStateStore(t)
	svc := &service{store: store, checkpointer: &fakeCheckpointer{}}
	_, err := svc.CheckpointContainer(context.Background(), &runtimev1.CheckpointContainerRequest{ContainerId: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("error code = %v, want NotFound", status.Code(err))
	}
}

func TestCheckpointContainerStoppedContainerFailedPrecondition(t *testing.T) {
	store := newCRIStateStore(t)
	ctx := context.Background()
	if err := store.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID: "sandbox-1",
		Namespace:    "default",
		PodNamespace: "team-a",
		Name:         "my-pod",
		State:        state.SandboxReady,
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	if err := store.UpsertContainer(ctx, state.ContainerRecord{
		ContainerID:  "container-1",
		PodSandboxID: "sandbox-1",
		Name:         "container-a",
		State:        state.ContainerExited,
	}); err != nil {
		t.Fatalf("UpsertContainer() error = %v", err)
	}
	cp := &fakeCheckpointer{}
	svc := &service{store: store, checkpointer: cp}

	_, err := svc.CheckpointContainer(ctx, &runtimev1.CheckpointContainerRequest{ContainerId: "container-1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("error code = %v, want FailedPrecondition", status.Code(err))
	}
	if cp.called {
		t.Fatal("checkpointer must not be called for a stopped container")
	}
}

func TestCheckpointContainerStoppedSandboxFailedPrecondition(t *testing.T) {
	store := newCRIStateStore(t)
	seedContainerAndSandbox(t, store, state.SandboxRecord{
		PodSandboxID: "sandbox-1",
		Namespace:    "default",
		PodNamespace: "team-a",
		Name:         "my-pod",
		State:        state.SandboxStopped,
	})
	cp := &fakeCheckpointer{}
	svc := &service{store: store, checkpointer: cp}

	_, err := svc.CheckpointContainer(context.Background(), &runtimev1.CheckpointContainerRequest{ContainerId: "container-1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("error code = %v, want FailedPrecondition", status.Code(err))
	}
	if cp.called {
		t.Fatal("checkpointer must not be called for a stopped sandbox")
	}
}
