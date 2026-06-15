package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/daemon/state"
)

// TestResolveSandboxByPod verifies the k8s (namespace, pod name) -> CRI sandbox
// id resolution that closes the snapshot-controller integration gap.
func TestResolveSandboxByPod(t *testing.T) {
	store, err := state.OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	seed := []state.SandboxRecord{
		{PodSandboxID: "cri-aaa", Name: "my-pod", PodNamespace: "team-a", Namespace: "k8s.io"},
		{PodSandboxID: "cri-bbb", Name: "other-pod", PodNamespace: "team-a", Namespace: "k8s.io"},
		// Same pod name in two different k8s namespaces (namespace disambiguates).
		{PodSandboxID: "cri-ccc", Name: "dup-pod", PodNamespace: "team-a", Namespace: "k8s.io"},
		{PodSandboxID: "cri-ddd", Name: "dup-pod", PodNamespace: "team-b", Namespace: "k8s.io"},
	}
	for _, rec := range seed {
		if err := store.UpsertSandbox(ctx, rec); err != nil {
			t.Fatalf("seed %s: %v", rec.PodSandboxID, err)
		}
	}

	s := &Daemon{stateStore: store}

	tests := []struct {
		name         string
		podNamespace string
		podName      string
		wantID       string
		wantErr      string
	}{
		{name: "exact match", podNamespace: "team-a", podName: "my-pod", wantID: "cri-aaa"},
		{name: "empty namespace matches unique name", podNamespace: "", podName: "my-pod", wantID: "cri-aaa"},
		{name: "namespace disambiguates duplicate names", podNamespace: "team-b", podName: "dup-pod", wantID: "cri-ddd"},
		{name: "not found", podNamespace: "team-a", podName: "ghost", wantErr: "no sandbox found"},
		{name: "namespace mismatch is not found", podNamespace: "team-z", podName: "my-pod", wantErr: "no sandbox found"},
		{name: "ambiguous without namespace", podNamespace: "", podName: "dup-pod", wantErr: "ambiguous"},
		{name: "empty pod name", podNamespace: "team-a", podName: "", wantErr: "pod_name is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := s.resolveSandboxByPod(ctx, tt.podNamespace, tt.podName)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (rec=%+v)", tt.wantErr, rec)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want contains %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.PodSandboxID != tt.wantID {
				t.Errorf("PodSandboxID = %q, want %q", rec.PodSandboxID, tt.wantID)
			}
		})
	}
}
