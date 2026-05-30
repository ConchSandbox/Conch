package state

import (
	"context"
	"testing"
)

func TestBoltStoreSandboxAndContainerCRUD(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sandbox := SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "pod-1",
		Namespace:      "default",
		Name:           "demo",
		State:          SandboxReady,
		Labels:         map[string]string{"app": "demo"},
	}
	if err := store.UpsertSandbox(ctx, sandbox); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	gotSandbox, err := store.GetSandbox(ctx, sandbox.PodSandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if gotSandbox.Name != sandbox.Name || gotSandbox.Labels["app"] != "demo" {
		t.Fatalf("GetSandbox() = %#v, want %#v", gotSandbox, sandbox)
	}

	container := ContainerRecord{
		ContainerID:  "ctr-1",
		PodSandboxID: sandbox.PodSandboxID,
		Name:         "placeholder",
		State:        ContainerCreated,
	}
	if err := store.UpsertContainer(ctx, container); err != nil {
		t.Fatalf("UpsertContainer() error = %v", err)
	}
	containers, err := store.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers() error = %v", err)
	}
	if len(containers) != 1 || containers[0].ContainerID != container.ContainerID {
		t.Fatalf("ListContainers() = %#v, want one container", containers)
	}

	if err := store.DeleteSandbox(ctx, sandbox.PodSandboxID); err != nil {
		t.Fatalf("DeleteSandbox() error = %v", err)
	}
	if _, err := store.GetSandbox(ctx, sandbox.PodSandboxID); err == nil {
		t.Fatalf("GetSandbox() after delete got nil error")
	}
}
