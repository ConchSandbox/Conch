package state

import (
	"context"
	"testing"
)

func TestBoltStoreSandboxCRUD(t *testing.T) {
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

	if err := store.DeleteSandbox(ctx, sandbox.PodSandboxID); err != nil {
		t.Fatalf("DeleteSandbox() error = %v", err)
	}
	if _, err := store.GetSandbox(ctx, sandbox.PodSandboxID); err == nil {
		t.Fatalf("GetSandbox() after delete got nil error")
	}
}
