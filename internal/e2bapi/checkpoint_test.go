package e2bapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	"github.com/openeuler/Conch/internal/conchruntime"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

// Only the VMM capture is simulated: the HTTP handler, runtime service,
// publication and named template storage are real. In this fork the Manager
// owns checkpoint orchestration, so the fake stands in for Manager.Checkpoint:
// it publishes the checkpoint Boot Index, advances the record head and invokes
// the register callback under the content lease.
type checkpointGuest struct {
	conchruntime.SandboxOps
	client *containerdclient.Client
	store  sandbox.Store
	t      *testing.T
	calls  []string
}

func (g *checkpointGuest) Checkpoint(ctx context.Context, sandboxID string, register func(context.Context, sandbox.CheckpointResult) error) (sandbox.CheckpointResult, error) {
	g.calls = append(g.calls, sandboxID)
	ctx = containerdclient.NewNamespaceContext(ctx)
	rec, err := g.store.Get(ctx, sandboxID)
	if err != nil {
		return sandbox.CheckpointResult{}, err
	}
	root := g.t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "memory"), []byte(fmt.Sprint(len(g.calls))), 0o600); err != nil {
		return sandbox.CheckpointResult{}, err
	}
	publishCtx, done, err := g.client.WithLease(ctx)
	if err != nil {
		return sandbox.CheckpointResult{}, err
	}
	defer done(publishCtx)
	leaseID, ok := leases.FromContext(publishCtx)
	if !ok {
		return sandbox.CheckpointResult{}, fmt.Errorf("checkpoint content lease is missing from context")
	}
	if err := g.client.LeasesService().AddResource(publishCtx, leases.Lease{ID: leaseID}, leases.Resource{
		Type: "content",
		ID:   rec.CheckpointHeadTemplateID,
	}); err != nil {
		return sandbox.CheckpointResult{}, err
	}
	parentID := rec.CheckpointHeadTemplateID
	published, err := conchimage.PublishCheckpointBootIndex(publishCtx, g.client, conchimage.PublishCheckpointBootIndexOptions{
		SourceBootIndexDigest: parentID,
		MemRoot:               root,
		VMMName:               "stratovirt",
		MemorySizeMB:          512,
		CPUCount:              rec.VCPUNum,
	})
	if err != nil {
		return sandbox.CheckpointResult{}, err
	}
	rec.CheckpointHeadTemplateID = published.BootIndexDigest
	if _, err := g.store.Update(publishCtx, rec); err != nil {
		return sandbox.CheckpointResult{}, err
	}
	result := sandbox.CheckpointResult{
		BootIndexDigest:       published.BootIndexDigest,
		ParentBootIndexDigest: parentID,
		Target:                published.Target,
	}
	if err := register(publishCtx, result); err != nil {
		return sandbox.CheckpointResult{}, err
	}
	return result, nil
}

func TestCheckpointTemplateMapping(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required")
	}
	// Use short paths for the embedded containerd Unix socket.
	root, err := os.MkdirTemp("", "e2b-checkpoint-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	host, err := containerdhost.Start(t.Context(), containerdhost.Config{
		RootDir: filepath.Join(root, "root"), StateDir: filepath.Join(root, "state"),
		Snapshot: containerdhost.SnapshotConfig{WorkDir: filepath.Join(root, "work")},
	})
	if err != nil {
		t.Skipf("embedded containerd host unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Error(err)
		}
	})
	service, routes := testRuntime(t)
	guest := &checkpointGuest{t: t, client: host.Client(), store: service.Store}
	service.Sandbox, service.Containerd, service.Templates = guest, host.Client(), host.TemplateStore()

	ctx, done, err := host.Client().WithLease(containerdclient.NewNamespaceContext(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer done(ctx)
	content := host.Client().ContentStore()
	rootfs, err := conchimage.BuildNativeComponentInContent(ctx, content, []string{t.TempDir()}, conchimage.KindRootfs)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := conchimage.BuildNativeComponentInContent(ctx, content, []string{t.TempDir()}, conchimage.KindSandbox)
	if err != nil {
		t.Fatal(err)
	}
	index, err := conchimage.BuildBootIndexInContent(ctx, content, conchimage.BootIndexContentOptions{
		RootfsDescriptor: rootfs, SandboxDescriptor: boot,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := seedRecord(t, service.Store, true, true, "checkpoint")
	before.CheckpointHeadTemplateID = index.Digest.String()
	before.VCPUNum = 2
	before, err = service.Store.Update(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	server := testServer(t, service, routes)
	var lastDigest string
	generated := map[string]bool{}
	for _, name := range []string{"ready:v1", "ready:v1", "", ""} {
		body := `{}`
		if name != "" {
			body = `{"name":"` + name + `"}`
		}
		resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+before.ID+"/snapshots", body, true)
		var result checkpointResponse
		if err := json.Unmarshal(data, &result); err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("checkpoint: HTTP %d %s (%v)", resp.StatusCode, data, err)
		}
		if name != "" {
			if result.SnapshotID != name || !reflect.DeepEqual(result.Names, []string{name}) {
				t.Fatalf("named response = %#v", result)
			}
		} else {
			if !strings.HasPrefix(result.SnapshotID, "checkpoint-") || generated[result.SnapshotID] || result.Names == nil || len(result.Names) != 0 {
				t.Fatalf("anonymous response = %#v", result)
			}
			if _, err := uuid.Parse(strings.TrimPrefix(result.SnapshotID, "checkpoint-")); err != nil {
				t.Fatal(err)
			}
			generated[result.SnapshotID] = true
		}
		entry, err := service.Templates.Get(ctx, result.SnapshotID)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Origin != conchtemplate.OriginCheckpoint || entry.BootMode != conchtemplate.BootModeResume || entry.SourceSandboxID != before.ID || entry.BootIndexDigest == lastDigest {
			t.Fatalf("published template = %#v, previous digest = %s", entry, lastDigest)
		}
		lastDigest = entry.BootIndexDigest
		after, err := service.Store.Get(ctx, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := before
		want.CheckpointHeadTemplateID = lastDigest
		if !reflect.DeepEqual(after, want) {
			t.Fatalf("checkpoint changed sandbox lifecycle: got %#v, want %#v", after, want)
		}
	}
	if len(guest.calls) != 4 {
		t.Fatalf("capture calls = %v", guest.calls)
	}
	for _, call := range guest.calls {
		if call != before.ID {
			t.Fatalf("wrong capture source: %s", call)
		}
	}
	entries, err := service.Templates.List(ctx, conchtemplate.Filter{Origin: conchtemplate.OriginCheckpoint})
	if err != nil || len(entries) != 3 {
		t.Fatalf("same-name update created extra templates: %v, %v", entries, err)
	}
}

func TestCheckpointRequestValidation(t *testing.T) {
	service, routes := testRuntime(t)
	ready := seedRecord(t, service.Store, true, true, "")
	private := seedRecord(t, service.Store, true, false, "")
	creating := seedRecord(t, service.Store, false, true, "")
	server := testServer(t, service, routes)
	for _, tc := range []struct {
		name, id, body string
		auth           bool
		status         int
	}{
		{"auth", ready.ID, `{}`, false, 401},
		{"native sandbox", private.ID, `{}`, true, 404},
		{"missing sandbox", uuid.NewString(), `{}`, true, 404},
		{"creating sandbox", creating.ID, `{}`, true, 409},
		{"null", ready.ID, `null`, true, 400},
		{"empty body", ready.ID, ``, true, 400},
		{"array", ready.ID, `[]`, true, 400},
		{"name type", ready.ID, `{"name":123}`, true, 400},
		{"empty name", ready.ID, `{"name":" "}`, true, 400},
		{"digest name", ready.ID, `{"name":"` + digest.FromString("snapshot").String() + `"}`, true, 400},
		{"unknown option", ready.ID, `{"name":"ready","fork":true}`, true, 400},
		{"extra body", ready.ID, `{} {}`, true, 400},
		{"too large", ready.ID, `{"name":"` + strings.Repeat("a", maxBodyBytes) + `"}`, true, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+tc.id+"/snapshots", tc.body, tc.auth)
			if resp.StatusCode != tc.status {
				t.Fatalf("got HTTP %d %s, want %d", resp.StatusCode, data, tc.status)
			}
		})
	}
	// Native checkpoint errors must not be reported as successful snapshots.
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/snapshots", `{}`, true)
	if resp.StatusCode != 500 || strings.Contains(string(data), "snapshotID") {
		t.Fatalf("unconfigured runtime = HTTP %d %s", resp.StatusCode, data)
	}
}
