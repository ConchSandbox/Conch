package committer

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name      string
		argv      []string
		wantMode  Mode
		wantPod   string
		wantNS    string
		wantSpecs []ContainerSpec
		wantErr   bool
	}{
		{
			name:      "commit single container, image uri carries a colon tag",
			argv:      []string{"my-pod", "team-a", "sandbox:registry.example.com/snap/abc:20260612-101530"},
			wantMode:  ModeCommit,
			wantPod:   "my-pod",
			wantNS:    "team-a",
			wantSpecs: []ContainerSpec{{Name: "sandbox", URI: "registry.example.com/snap/abc:20260612-101530"}},
		},
		{
			name:     "commit multiple containers",
			argv:     []string{"p", "n", "c1:r/a:t", "c2:r/b:t"},
			wantMode: ModeCommit,
			wantPod:  "p",
			wantNS:   "n",
			wantSpecs: []ContainerSpec{
				{Name: "c1", URI: "r/a:t"},
				{Name: "c2", URI: "r/b:t"},
			},
		},
		{
			name:      "unpause",
			argv:      []string{"unpause", "my-pod", "team-a", "sandbox"},
			wantMode:  ModeUnpause,
			wantPod:   "my-pod",
			wantNS:    "team-a",
			wantSpecs: []ContainerSpec{{Name: "sandbox"}},
		},
		{name: "commit missing args", argv: []string{"p", "n"}, wantErr: true},
		{name: "commit bad spec no colon", argv: []string{"p", "n", "noColon"}, wantErr: true},
		{name: "unpause missing container", argv: []string{"unpause", "p", "n"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseArgs(tt.argv)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (parsed %+v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Mode != tt.wantMode || got.PodName != tt.wantPod || got.Namespace != tt.wantNS {
				t.Fatalf("got mode=%v pod=%q ns=%q", got.Mode, got.PodName, got.Namespace)
			}
			if len(got.Containers) != len(tt.wantSpecs) {
				t.Fatalf("got %d specs, want %d", len(got.Containers), len(tt.wantSpecs))
			}
			for i := range tt.wantSpecs {
				if got.Containers[i] != tt.wantSpecs[i] {
					t.Errorf("spec[%d] = %+v, want %+v", i, got.Containers[i], tt.wantSpecs[i])
				}
			}
		})
	}
}

// TestRunCommit_EndToEnd mimics the OpenSandbox commit Job against a mock conchd:
// the shim must send the right checkpoint request and write a termination message
// that matches OpenSandbox's commitJobResult shape.
func TestRunCommit_EndToEnd(t *testing.T) {
	var gotPath string
	var gotReq CheckpointRequest
	conchd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(CheckpointResponse{
			ImageRef:    gotReq.ImageRef,
			ImageDigest: "sha256:abc123",
		})
	}))
	defer conchd.Close()

	dir := t.TempDir()
	termPath := filepath.Join(dir, "termination-log")

	client := &Client{HTTP: conchd.Client(), BaseURL: conchd.URL}
	args := Args{
		Mode:       ModeCommit,
		PodName:    "my-pod",
		Namespace:  "team-a",
		Containers: []ContainerSpec{{Name: "sandbox", URI: "registry.example.com/snap/abc:tag"}},
	}
	cfg := Config{PlainHTTP: true, TerminationMessagePath: termPath}

	if err := RunCommit(context.Background(), client, cfg, args); err != nil {
		t.Fatalf("RunCommit: %v", err)
	}

	// (1) shim sends k8s pod_name, not a faked sandbox_id.
	if gotPath != "/api/snapshot/checkpoint" {
		t.Errorf("conchd path = %q, want /api/snapshot/checkpoint", gotPath)
	}
	if gotReq.PodName != "my-pod" {
		t.Errorf("pod_name = %q, want my-pod", gotReq.PodName)
	}
	if gotReq.SandboxID != "" {
		t.Errorf("sandbox_id = %q, want empty (resolved by conchd from pod_name)", gotReq.SandboxID)
	}
	if gotReq.Namespace != "team-a" {
		t.Errorf("namespace = %q, want team-a", gotReq.Namespace)
	}
	if !gotReq.PlainHTTP {
		t.Errorf("plain_http = false, want true")
	}

	// (2) termination message matches OpenSandbox's commitJobResult format.
	data, err := os.ReadFile(termPath)
	if err != nil {
		t.Fatalf("read termination message: %v", err)
	}
	var result snapshotResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("termination message is not valid commitJobResult JSON: %v\nraw: %s", err, data)
	}
	if len(result.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(result.Containers))
	}
	if got := result.Containers[0]; got.Name != "sandbox" || got.Digest != "sha256:abc123" {
		t.Errorf("container result = %+v, want name=sandbox digest=sha256:abc123", got)
	}
}

// TestNewUnixClient_RoundTrip exercises the real transport: dialing conchd over a
// unix socket.
func TestNewUnixClient_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "conchd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CheckpointResponse{ImageRef: "r/a:t", ImageDigest: "sha256:deadbeef"})
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	client := NewUnixClient(sock)
	resp, err := client.Checkpoint(context.Background(), CheckpointRequest{SandboxID: "s", ImageRef: "r/a:t"})
	if err != nil {
		t.Fatalf("Checkpoint over unix socket: %v", err)
	}
	if resp.ImageDigest != "sha256:deadbeef" {
		t.Errorf("digest = %q, want sha256:deadbeef", resp.ImageDigest)
	}
}
