package cri

import (
	"context"
	"testing"

	"github.com/openeuler/Conch/internal/daemon/state"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestCreateContainerUsesImageAsPlaceholderImageRef(t *testing.T) {
	runtime := &fakeRuntime{}
	svc := &service{runtime: runtime}

	_, err := svc.CreateContainer(context.Background(), &runtimev1.CreateContainerRequest{
		PodSandboxId: "sandbox-1",
		Config: &runtimev1.ContainerConfig{
			Metadata: &runtimev1.ContainerMetadata{Name: "container-a"},
			Image:    &runtimev1.ImageSpec{Image: "registry.example.invalid/app:latest"},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}
	if runtime.createContainerReq.Image != "registry.example.invalid/app:latest" {
		t.Fatalf("Image = %q", runtime.createContainerReq.Image)
	}
	if runtime.createContainerReq.ImageRef != "registry.example.invalid/app:latest" {
		t.Fatalf("ImageRef = %q, want image name placeholder ref", runtime.createContainerReq.ImageRef)
	}
}

func TestContainerStatusFallsBackToImageWhenImageRefMissing(t *testing.T) {
	ctx := context.Background()
	store := newCRIStateStore(t)
	if err := store.UpsertContainer(ctx, state.ContainerRecord{
		ContainerID:  "container-1",
		PodSandboxID: "sandbox-1",
		Name:         "container-a",
		Image:        "registry.example.invalid/app:latest",
		State:        state.ContainerRunning,
	}); err != nil {
		t.Fatalf("UpsertContainer() error = %v", err)
	}
	svc := &service{store: store}

	resp, err := svc.ContainerStatus(ctx, &runtimev1.ContainerStatusRequest{ContainerId: "container-1"})
	if err != nil {
		t.Fatalf("ContainerStatus() error = %v", err)
	}
	if resp.GetStatus().GetImageRef() != "registry.example.invalid/app:latest" {
		t.Fatalf("ImageRef = %q, want image fallback", resp.GetStatus().GetImageRef())
	}
	if resp.GetStatus().GetImageId() != "registry.example.invalid/app:latest" {
		t.Fatalf("ImageId = %q, want image fallback", resp.GetStatus().GetImageId())
	}
}

func newCRIStateStore(t *testing.T) *state.BoltStore {
	t.Helper()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return store
}
