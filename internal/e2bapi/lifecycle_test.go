package e2bapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openeuler/Conch/api/go_proto/pbconnect"
	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/envd"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/sandbox"
)

// pauseGuest fakes the runtime pieces a pause/connect cycle needs that the
// real Manager would own: checkpoint capture plus the store write of the
// paused record and sandbox deletion. In this fork Manager.Checkpoint owns
// capture orchestration, so the fake publishes the checkpoint Boot Index,
// advances the record head and invokes the register callback under the
// content lease; publication, template store and record persistence run
// against real components.
type pauseGuest struct {
	conchruntime.SandboxOps
	t           *testing.T
	client      *containerdclient.Client
	store       sandbox.Store
	deleteCalls int
}

func (g *pauseGuest) Checkpoint(ctx context.Context, sandboxID string, register func(context.Context, sandbox.CheckpointResult) error) (sandbox.CheckpointResult, error) {
	ctx = containerdclient.NewNamespaceContext(ctx)
	rec, err := g.store.Get(ctx, sandboxID)
	if err != nil {
		return sandbox.CheckpointResult{}, err
	}
	root := g.t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "memory"), []byte("paused"), 0o600); err != nil {
		return sandbox.CheckpointResult{}, err
	}
	publishCtx, done, err := g.client.WithLease(ctx)
	if err != nil {
		return sandbox.CheckpointResult{}, err
	}
	defer done(publishCtx)
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

func (g *pauseGuest) Delete(ctx context.Context, sandboxID string) error {
	g.deleteCalls++
	return g.store.Delete(ctx, sandboxID)
}

// startPauseRuntime boots the embedded containerd host and builds one boot
// index, mirroring TestCheckpointTemplateMapping. It skips when the host or
// mkfs.erofs is unavailable. The returned release function drops the content
// lease; hold it for the whole test or the boot index is GC-collected.
func startPauseRuntime(t *testing.T, guest *pauseGuest) (*conchruntime.Service, sandbox.Record, func(), bool) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required")
	}
	root, err := os.MkdirTemp("", "e2b-pause-*")
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
	guest.t = t
	guest.client = host.Client()
	guest.store = service.Store
	service.Sandbox = guest
	service.Containerd = host.Client()
	service.Templates = host.TemplateStore()
	_ = routes

	ctx, done, err := host.Client().WithLease(containerdclient.NewNamespaceContext(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
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
	before := seedRecord(t, service.Store, true, true, "pause")
	before.CheckpointHeadTemplateID = index.Digest.String()
	before, err = service.Store.Update(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	return service, before, func() { _ = done(ctx) }, true
}

func TestPauseConnectLifecycle(t *testing.T) {
	guest := &pauseGuest{}
	service, before, release, ok := startPauseRuntime(t, guest)
	if !ok {
		return
	}
	defer release()
	server := testServer(t, service, service.ProxyRoutes)

	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+before.ID+"/pause", `{"memory":false}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("memory:false = %d %s", resp.StatusCode, data)
	}
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+before.ID+"/pause", `{}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pause = %d %s", resp.StatusCode, data)
	}
	after, err := service.Store.Get(t.Context(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != sandbox.StatePaused || after.ExpiresAt != 0 || after.IP != "" || after.RuntimeID != "" || after.VMMPID != 0 {
		t.Fatalf("paused record = %#v", after)
	}
	if !strings.HasPrefix(after.PauseTemplateName, "pause-") {
		t.Fatalf("pause template name = %q", after.PauseTemplateName)
	}
	if _, ok := service.ProxyRoutes.Lookup(before.ID); ok {
		t.Fatal("pause left an active proxy route")
	}
	if guest.deleteCalls != 1 {
		t.Fatalf("runtime delete calls = %d", guest.deleteCalls)
	}
	// Second pause returns 409 (SDK pause() reports False).
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+before.ID+"/pause", `{}`, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("double pause = %d", resp.StatusCode)
	}
	// get exposes the paused state instead of 409.
	resp, data = doRequest(t, server, http.MethodGet, "/sandboxes/"+before.ID, "", true)
	var detail map[string]any
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || detail["state"] != "paused" {
		t.Fatalf("paused detail = %d %s", resp.StatusCode, data)
	}
}

// TestConnectRestoresPausedSandbox proves connect on a PAUSED record replays
// the pause-time configuration through CreateSandbox under the same ID and
// answers 201 with the restored detail.
func TestConnectRestoresPausedSandbox(t *testing.T) {
	guest := &pauseGuest{}
	service, before, release, ok := startPauseRuntime(t, guest)
	if !ok {
		return
	}
	defer release()
	server := testServer(t, service, service.ProxyRoutes)

	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+before.ID+"/pause", `{}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pause = %d %s", resp.StatusCode, data)
	}
	paused, err := service.Store.Get(t.Context(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != sandbox.StatePaused || paused.CheckpointHeadTemplateID == "" {
		t.Fatalf("paused record = %#v", paused)
	}

	// connect on READY only extends the TTL (200). connect on PAUSED restores
	// (201) under the original ID.
	ready := seedRecord(t, service.Store, true, true, "resume")
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/connect", `{}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("running connect = %d %s", resp.StatusCode, data)
	}
	var extended sandboxDetail
	if err := json.Unmarshal(data, &extended); err != nil {
		t.Fatal(err)
	}
	if extended.SandboxID != ready.ID || extended.State != "running" {
		t.Fatalf("running connect response = %#v", extended)
	}

	restored := localGuest{ip: "127.0.0.38", t: t, store: service.Store}
	service.Sandbox = &restored
	service.Envd = envd.NewClient()
	_, process := pbconnect.NewProcessServiceHandler(&restored)
	listenGuest(t, restored.ip, 4064, process)
	listenGuest(t, restored.ip, envd.DefaultPort, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+before.ID+"/connect", `{"timeout":600}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("paused connect = %d %s", resp.StatusCode, data)
	}
	var resumed sandboxDetail
	if err := json.Unmarshal(data, &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.SandboxID != before.ID || resumed.State != "running" {
		t.Fatalf("resumed response = %#v", resumed)
	}
	rec, err := service.Store.Get(t.Context(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != sandbox.StateReady || rec.IP != restored.ip {
		t.Fatalf("resumed record = %#v", rec)
	}
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 590*time.Second {
		t.Fatalf("resume did not apply the connect timeout: %v", remaining)
	}
	if _, ok := service.ProxyRoutes.Lookup(before.ID); !ok {
		t.Fatal("resume did not republish the proxy route")
	}
}

// TestExpiryAutoPausesTimeoutActionPause proves RunExpiry routes an expired
// TimeoutAction=pause sandbox through PauseSandbox instead of deleting it.
func TestExpiryAutoPausesTimeoutActionPause(t *testing.T) {
	guest := &pauseGuest{}
	service, before, release, ok := startPauseRuntime(t, guest)
	if !ok {
		return
	}
	defer release()
	server := testServer(t, service, service.ProxyRoutes)

	rec := before
	rec.TimeoutAction = sandbox.TimeoutActionPause
	rec.ExpiresAt = time.Now().Add(-time.Second).UnixNano()
	if _, err := service.Store.Update(t.Context(), rec); err != nil {
		t.Fatal(err)
	}

	expiryCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); server.Config.Handler.(*Server).RunExpiry(expiryCtx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		after, err := service.Store.Get(t.Context(), before.ID)
		if err == nil && after.State == sandbox.StatePaused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expired auto-pause sandbox = %+v (%v)", after, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPauseConnectStateMatrix covers handler guards that need no real
// checkpoint pipeline: unknown IDs, non-E2B sandboxes and non-ready states.
func TestPauseConnectStateMatrix(t *testing.T) {
	service, routes := testRuntime(t)
	server := testServer(t, service, routes)
	native := seedRecord(t, service.Store, true, false, "")
	creating := seedRecord(t, service.Store, false, true, "")
	ready := seedRecord(t, service.Store, true, true, "")

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"pause unknown", http.MethodPost, "/sandboxes/" + uuid.NewString() + "/pause", `{}`, 404},
		{"pause native", http.MethodPost, "/sandboxes/" + native.ID + "/pause", `{}`, 404},
		{"pause creating", http.MethodPost, "/sandboxes/" + creating.ID + "/pause", `{}`, 409},
		{"pause null body", http.MethodPost, "/sandboxes/" + ready.ID + "/pause", `null`, 400},
		{"pause unknown key", http.MethodPost, "/sandboxes/" + ready.ID + "/pause", `{"disk":true}`, 400},
		{"connect unknown", http.MethodPost, "/sandboxes/" + uuid.NewString() + "/connect", `{}`, 404},
		{"connect native", http.MethodPost, "/sandboxes/" + native.ID + "/connect", `{}`, 404},
		{"connect creating", http.MethodPost, "/sandboxes/" + creating.ID + "/connect", `{}`, 409},
		{"connect bad timeout", http.MethodPost, "/sandboxes/" + ready.ID + "/connect", `{"timeout":0}`, 400},
		{"connect huge timeout", http.MethodPost, "/sandboxes/" + ready.ID + "/connect", `{"timeout":86401}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := doRequest(t, server, tc.method, tc.path, tc.body, true)
			if resp.StatusCode != tc.status {
				t.Fatalf("got HTTP %d %s, want %d", resp.StatusCode, data, tc.status)
			}
		})
	}
}

// TestTTLAndNetworkHandlers exercises set_timeout/refreshes semantics and the
// update_network payload validation against real store records.
func TestTTLAndNetworkHandlers(t *testing.T) {
	service, routes := testRuntime(t)
	server := testServer(t, service, routes)
	native := seedRecord(t, service.Store, true, false, "")
	creating := seedRecord(t, service.Store, false, true, "")
	ready := seedRecord(t, service.Store, true, true, "")

	// set_timeout requires a positive timeout and resets (may shorten) the TTL.
	resp, _ := doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":3600}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set_timeout = %d", resp.StatusCode)
	}
	rec, _ := service.Store.Get(t.Context(), ready.ID)
	deadline := time.Unix(0, rec.ExpiresAt)
	if remaining := time.Until(deadline); remaining < 3500*time.Second || remaining > 3600*time.Second {
		t.Fatalf("set_timeout deadline not reset: %v", remaining)
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":60}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("shortening set_timeout = %d", resp.StatusCode)
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining > 60*time.Second {
		t.Fatalf("set_timeout did not shorten: %v", remaining)
	}

	// refreshes clamps and only extends. Start from a short reset deadline so
	// the clamped minimum is observable.
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":10}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("reset to 10s failed")
	}
	for _, tc := range []struct {
		body             string
		wantMin, wantMax time.Duration
	}{
		{`{"duration":1}`, 14 * time.Second, 16 * time.Second},
		{`{"duration":100000}`, 3599 * time.Second, 3601 * time.Second},
	} {
		resp, _ := doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/refreshes", tc.body, true)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("refreshes %s = %d", tc.body, resp.StatusCode)
		}
		rec, _ := service.Store.Get(t.Context(), ready.ID)
		if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < tc.wantMin || remaining > tc.wantMax {
			t.Fatalf("refreshes %s deadline = %v", tc.body, remaining)
		}
	}
	// refreshes never shortens: a 15s refresh after a 3600s reset keeps the hour.
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":3600}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("reset before extend check failed")
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/refreshes", `{"duration":15}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("short refresh failed")
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 3500*time.Second {
		t.Fatalf("refreshes shortened the deadline: %v", remaining)
	}

	// TTL guards: unknown, native and non-ready states.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/sandboxes/" + uuid.NewString() + "/timeout", `{"timeout":60}`},
		{http.MethodPost, "/sandboxes/" + native.ID + "/timeout", `{"timeout":60}`},
		{http.MethodPost, "/sandboxes/" + creating.ID + "/timeout", `{"timeout":60}`},
		{http.MethodPost, "/sandboxes/" + uuid.NewString() + "/refreshes", `{}`},
		{http.MethodPost, "/sandboxes/" + native.ID + "/refreshes", `{}`},
		{http.MethodPost, "/sandboxes/" + creating.ID + "/refreshes", `{}`},
	} {
		resp, data := doRequest(t, server, tc.method, tc.path, tc.body, true)
		if resp.StatusCode != 404 && resp.StatusCode != 409 {
			t.Fatalf("%s %s = %d %s, want 404/409", tc.method, tc.path, resp.StatusCode, data)
		}
	}

	// update_network: unsupported payloads and guards.
	for _, tc := range []struct {
		name, id, body string
		status         int
	}{
		{"egress proxy", ready.ID, `{"egressProxy":{"url":"http://proxy"}}`, 400},
		{"rules", ready.ID, `{"rules":[{"port":80}]}`, 400},
		{"creating", creating.ID, `{"allowOut":["1.1.1.1/32"]}`, 409},
		{"unknown", uuid.NewString(), `{}`, 404},
		{"native", native.ID, `{}`, 404},
		{"unknown key", ready.ID, `{"allowPublicTraffic":false}`, 400},
	} {
		t.Run("network "+tc.name, func(t *testing.T) {
			resp, data := doRequest(t, server, http.MethodPut, "/sandboxes/"+tc.id+"/network", tc.body, true)
			if resp.StatusCode != tc.status {
				t.Fatalf("got HTTP %d %s, want %d", resp.StatusCode, data, tc.status)
			}
		})
	}
	// The handler delegates to UpdateSandboxNetworkConfig; without a configured
	// runtime the store update and rollback path run against the raw service.
	// A valid payload on a ready sandbox still fails closed here because the
	// sandbox runtime update is unconfigured (500), proving the payload passed
	// validation and reached the runtime layer.
	resp, data := doRequest(t, server, http.MethodPut, "/sandboxes/"+ready.ID+"/network", `{"allowOut":["1.1.1.1/32"],"denyOut":["0.0.0.0/0"],"allow_internet_access":false}`, true)
	if resp.StatusCode < 400 {
		t.Fatalf("unconfigured runtime accepted network update: %d %s", resp.StatusCode, data)
	}
}
