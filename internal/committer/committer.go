// Package committer implements conch-committer: a drop-in replacement for
// OpenSandbox's image-committer that drives Conch's whole-VM snapshot (conchd
// POST /api/snapshot/checkpoint) instead of "nerdctl commit + push". It honours
// the same Job contract OpenSandbox expects (argv/env in, termination-message
// out), so OpenSandbox's snapshot CRD/Controller is reused unmodified — point
// the controller at this image via --image-committer-image.
package committer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultTerminationMessagePath must match the path OpenSandbox's controller
// reads the commit result from.
const DefaultTerminationMessagePath = "/dev/termination-log"

type Mode int

const (
	ModeCommit Mode = iota
	ModeUnpause
)

// ContainerSpec is one "<name>:<imageURI>" positional argument.
type ContainerSpec struct {
	Name string
	URI  string
}

type Args struct {
	Mode       Mode
	PodName    string
	Namespace  string
	Containers []ContainerSpec
}

// ParseArgs parses the OpenSandbox image-committer argument contract:
//
//	commit:  <pod_name> <namespace> <container1:uri1> [container2:uri2...]
//	unpause: unpause <pod_name> <namespace> <container_name> [container_name...]
func ParseArgs(argv []string) (Args, error) {
	if len(argv) > 0 && argv[0] == "unpause" {
		rest := argv[1:]
		if len(rest) < 3 {
			return Args{}, fmt.Errorf("unpause requires <pod_name> <namespace> <container_name>...")
		}
		cs := make([]ContainerSpec, 0, len(rest)-2)
		for _, name := range rest[2:] {
			cs = append(cs, ContainerSpec{Name: name})
		}
		return Args{Mode: ModeUnpause, PodName: rest[0], Namespace: rest[1], Containers: cs}, nil
	}

	if len(argv) < 3 {
		return Args{}, fmt.Errorf("commit requires <pod_name> <namespace> <container:imageURI>...")
	}
	cs := make([]ContainerSpec, 0, len(argv)-2)
	for _, raw := range argv[2:] {
		// Split on the first colon: container names have none, the image URI has
		// one before its tag.
		i := strings.Index(raw, ":")
		if i <= 0 || i == len(raw)-1 {
			return Args{}, fmt.Errorf("invalid container spec %q, want <name>:<imageURI>", raw)
		}
		cs = append(cs, ContainerSpec{Name: raw[:i], URI: raw[i+1:]})
	}
	return Args{Mode: ModeCommit, PodName: argv[0], Namespace: argv[1], Containers: cs}, nil
}

// CheckpointRequest / CheckpointResponse mirror conchd's /api/snapshot/checkpoint.
// Exactly one of SandboxID / PodName targets the VM; we send PodName and let
// conchd resolve the CRI sandbox id, SandboxID being an explicit override.
type CheckpointRequest struct {
	Namespace string `json:"namespace,omitempty"`
	SandboxID string `json:"sandbox_id,omitempty"`
	PodName   string `json:"pod_name,omitempty"`
	ImageRef  string `json:"image_ref"`
	PlainHTTP bool   `json:"plain_http,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
}

type CheckpointResponse struct {
	ImageRef    string `json:"image_ref"`
	ImageDigest string `json:"image_digest"`
}

// Client talks to a conchd HTTP API (over a unix socket or TCP).
type Client struct {
	HTTP    *http.Client
	BaseURL string
}

// NewUnixClient dials conchd over its unix socket (the real deployment path: the
// commit Job hostPath-mounts the socket and exposes it via CONTAINERD_SOCKET).
func NewUnixClient(socketPath string) *Client {
	return &Client{
		HTTP: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
			Timeout: 30 * time.Minute, // a whole-VM checkpoint + push can be slow
		},
		BaseURL: "http://conchd", // host ignored for unix sockets
	}
}

func (c *Client) Checkpoint(ctx context.Context, req CheckpointRequest) (CheckpointResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return CheckpointResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/snapshot/checkpoint", bytes.NewReader(body))
	if err != nil {
		return CheckpointResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return CheckpointResponse{}, fmt.Errorf("call conchd checkpoint: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return CheckpointResponse{}, fmt.Errorf("conchd checkpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out CheckpointResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return CheckpointResponse{}, fmt.Errorf("decode conchd response: %w", err)
	}
	return out, nil
}

// Config carries environment-derived settings for a committer run.
type Config struct {
	SandboxID              string // explicit override; defaults to the pod name
	PlainHTTP              bool
	Username               string
	Password               string
	TerminationMessagePath string // defaults to DefaultTerminationMessagePath
}

func (c Config) terminationPath() string {
	if c.TerminationMessagePath != "" {
		return c.TerminationMessagePath
	}
	return DefaultTerminationMessagePath
}

// snapshotResult must match OpenSandbox's commitJobResult JSON (snapshotResultFromPod),
// or the controller cannot read the digest back.
type snapshotResult struct {
	Containers []snapshotContainerResult `json:"containers"`
}

type snapshotContainerResult struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// RunCommit drives a Conch whole-VM checkpoint and writes the OpenSandbox-format
// result to the termination message.
func RunCommit(ctx context.Context, client *Client, cfg Config, a Args) error {
	if len(a.Containers) == 0 {
		return fmt.Errorf("no container specs to commit")
	}

	// Conch snapshots the whole VM as one image; OpenSandbox may pass several
	// per-container specs, so checkpoint once against the first and report that
	// digest for every requested container.
	primary := a.Containers[0]
	if len(a.Containers) > 1 {
		fmt.Fprintf(os.Stderr, "WARNING: Conch snapshots the whole VM; using %q for all %d container specs\n",
			primary.URI, len(a.Containers))
	}

	req := CheckpointRequest{
		Namespace: a.Namespace,
		ImageRef:  primary.URI,
		PlainHTTP: cfg.PlainHTTP,
		Username:  cfg.Username,
		Password:  cfg.Password,
	}
	if cfg.SandboxID != "" {
		req.SandboxID = cfg.SandboxID
		fmt.Printf("conch-committer: checkpointing sandbox=%s ns=%s -> %s\n", req.SandboxID, a.Namespace, primary.URI)
	} else {
		req.PodName = a.PodName // conchd resolves the CRI sandbox id from the pod name
		fmt.Printf("conch-committer: checkpointing pod=%s ns=%s -> %s\n", req.PodName, a.Namespace, primary.URI)
	}

	resp, err := client.Checkpoint(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("conch-committer: checkpoint done image=%s digest=%s\n", resp.ImageRef, resp.ImageDigest)

	result := snapshotResult{Containers: make([]snapshotContainerResult, 0, len(a.Containers))}
	for _, cs := range a.Containers {
		result.Containers = append(result.Containers, snapshotContainerResult{
			Name:   cs.Name,
			Image:  resp.ImageRef,
			Digest: resp.ImageDigest,
		})
	}
	return writeTerminationMessage(cfg.terminationPath(), result)
}

// RunUnpause is a no-op: Conch restores via Pod recreation with the
// conch.io/use-snapshot annotation, not by unpausing the source, and conchd has
// no resume endpoint.
func RunUnpause(a Args) error {
	fmt.Printf("conch-committer: unpause is a no-op for Conch (restore is via Pod recreation); pod=%s ns=%s\n",
		a.PodName, a.Namespace)
	return nil
}

func writeTerminationMessage(path string, result snapshotResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
