package e2bapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdsandbox "github.com/openeuler/Conch/internal/adapters/containerd/sandbox"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandboxproxy"
	conchtemplate "github.com/openeuler/Conch/internal/template"
	bolt "go.etcd.io/bbolt"
)

const testAPIKey = "node-secret"

func testRuntime(t *testing.T) (*conchruntime.Service, *sandboxproxy.Registry) {
	t.Helper()
	root := t.TempDir()
	bdb, err := bolt.Open(filepath.Join(root, "metadata.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bdb.Close() })
	content, err := local.NewStore(filepath.Join(root, "content"))
	if err != nil {
		t.Fatal(err)
	}
	db := metadata.NewDB(bdb, content, nil)
	if err := db.Init(containerdclient.NewNamespaceContext(t.Context())); err != nil {
		t.Fatal(err)
	}
	store := containerdsandbox.NewStore(metadata.NewSandboxStore(db))
	service := conchruntime.New(nil, nil)
	service.Store = store
	routes := sandboxproxy.NewRegistry()
	t.Cleanup(routes.Close)
	return service, routes
}

func testServer(t *testing.T, service *conchruntime.Service, routes *sandboxproxy.Registry) *httptest.Server {
	t.Helper()
	api, err := New(Config{APIKey: testAPIKey, Domains: []string{"sandbox.example.test"}, RequestTimeout: 5 * time.Second}, service, routes)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	return server
}

func doRequest(t *testing.T, server *httptest.Server, method, path, body string, auth bool) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("X-API-Key", testAPIKey)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func seedRecord(t *testing.T, store sandbox.Store, ready bool, e2b bool, group string) sandbox.Record {
	t.Helper()
	state := sandbox.StateReady
	if !ready {
		state = sandbox.StateCreating
	}
	digest := digest.FromString("preinstalled-template").String()
	record, err := store.Create(t.Context(), sandbox.Record{
		ID: uuid.NewString(), State: state, SourceTemplateName: "base", SourceTemplateID: digest, CheckpointHeadTemplateID: digest,
		IP: "192.0.2.10", VCPUNum: 2, RamMB: 512, E2B: e2b, EnvdVersion: "0.8.3", Metadata: map[string]string{"group": group},
		ExpiresAt: time.Now().Add(5 * time.Minute).UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestAuthenticationAndProxyClassification(t *testing.T) {
	service, routes := testRuntime(t)
	server := testServer(t, service, routes)
	for _, path := range []string{"/sandboxes", "/v2/sandboxes", "/sandboxes/" + uuid.NewString(), "/unknown"} {
		resp, _ := doRequest(t, server, http.MethodGet, path, "", false)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s = %d", path, resp.StatusCode)
		}
	}
	resp, _ := doRequest(t, server, http.MethodGet, "/health", "", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Node health status = %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/sandboxes", nil)
	req.Header.Add("X-API-Key", testAPIKey)
	req.Header.Add("X-API-Key", "another-key")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("duplicate API keys authorized: HTTP %d", resp.StatusCode)
	}
	// A wrong key of a different length stays rejected; the digest comparison
	// does not disturb the correct key.
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/sandboxes", nil)
	req.Header.Set("X-API-Key", "wrong-key-of-a-different-length")
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-length key authorized: HTTP %d", resp.StatusCode)
	}
	resp, data := doRequest(t, server, http.MethodGet, "/sandboxes", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct key rejected: %d %s", resp.StatusCode, data)
	}
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			t.Error("proxy leaked Node API key to guest")
		}
		fmt.Fprint(w, "guest:"+r.URL.Path)
	}))
	defer guest.Close()
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)
	id := uuid.NewString()
	routes.Insert(id)
	if err := routes.Publish(id, ip, ""); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ path, host, expected string }{
		{"/proxy/health", "", "guest:/health"},
		{"/files", "", "guest:/files"},
		{"/sandboxes", port + "-" + id + ".sandbox.example.test", "guest:/sandboxes"},
	} {
		req, _ := http.NewRequest(http.MethodGet, server.URL+test.path, nil)
		req.Host = test.host
		req.Header.Set(sandboxproxy.E2BSandboxIDHeader, id)
		req.Header.Set(sandboxproxy.E2BTargetPortHeader, port)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != test.expected {
			t.Errorf("proxy %s host=%s: %d %q", test.path, test.host, resp.StatusCode, body)
		}
	}
	// A routing header on a concrete control path must not bypass API auth.
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/sandboxes", nil)
	req.Header.Set(sandboxproxy.SandboxIDHeader, id)
	req.Header.Set(sandboxproxy.TargetPortHeader, port)
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("routing headers bypassed control auth: %d", resp.StatusCode)
	}
}

func TestUnsupportedAndMalformedRequests(t *testing.T) {
	service, routes := testRuntime(t)
	server := testServer(t, service, routes)
	for _, operation := range []string{"pause", "resume", "connect"} {
		resp, body := doRequest(t, server, http.MethodPost, "/sandboxes/"+uuid.NewString()+"/"+operation, `{}`, true)
		if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "Unimplemented") {
			t.Errorf("%s = %d %q", operation, resp.StatusCode, body)
		}
	}
	for _, body := range []string{
		`{"templateID":"base","secure":true}`,
		`{"templateID":"base"}`,
		`{"templateID":"base","secure":false,"autoPause":true}`,
		`{"templateID":"base","secure":false,"autoResume":{"enabled":true}}`,
		`{"templateID":"base","secure":false,"network":{"allowPublicTraffic":false}}`,
		`{"templateID":"base","secure":false,"network":{"maskRequestHost":"example.com"}}`,
		`{"templateID":"base","secure":false,"volumeMounts":[{}]}`,
	} {
		resp, data := doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("unsupported %s = %d %q", body, resp.StatusCode, data)
		}
	}
	for _, body := range []string{
		`{`, `null`, `{}`, `{"templateID":"base","secure":"false"}`,
		`{"templateID":"base","secure":false} {}`,
		`{"templateID":"base","secure":false,"cpuCount":2}`,
		`{"templateID":"base","secure":false,"timeout":-1}`,
		`{"templateID":"base","secure":false,"timeout":0}`,
		`{"templateID":"base","secure":false,"timeout":2147483648}`,
		`{"templateID":"base","secure":false,"network":{"private":true}}`,
	} {
		resp, data := doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("malformed %s = %d %q", body, resp.StatusCode, data)
		}
	}
	resp, _ := doRequest(t, server, http.MethodPost, "/sandboxes", `{"templateID":"`+strings.Repeat("a", maxBodyBytes)+`"}`, true)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized create = %d", resp.StatusCode)
	}
}

func TestDetailAndClusterListContract(t *testing.T) {
	service, routes := testRuntime(t)
	server := testServer(t, service, routes)
	var records []sandbox.Record
	for i := range 105 {
		group := "other"
		if i%2 == 0 {
			group = "selected"
		}
		records = append(records, seedRecord(t, service.Store, true, true, group))
	}
	creating := seedRecord(t, service.Store, false, true, "selected")
	privateRecord := seedRecord(t, service.Store, true, false, "selected")
	resp, data := doRequest(t, server, http.MethodGet, "/sandboxes/"+records[0].ID, "", true)
	var detail map[string]any
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || detail["sandboxID"] != records[0].ID || detail["envdVersion"] != "0.8.3" || detail["state"] != "running" || detail["domain"] != "sandbox.example.test" || detail["cpuCount"] != float64(2) || detail["memoryMB"] != float64(512) {
		t.Fatalf("detail = %d %s", resp.StatusCode, data)
	}
	for _, field := range []string{"templateID", "sandboxID", "clientID", "envdVersion", "startedAt", "endAt", "cpuCount", "memoryMB", "diskSizeMB", "state", "metadata"} {
		if _, ok := detail[field]; !ok {
			t.Errorf("missing required detail field %s", field)
		}
	}
	resp, _ = doRequest(t, server, http.MethodGet, "/sandboxes/"+creating.ID, "", true)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("creating sandbox GET = %d, want 409", resp.StatusCode)
	}
	resp, _ = doRequest(t, server, http.MethodGet, "/sandboxes/"+privateRecord.ID, "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("native-only sandbox exposed: HTTP %d", resp.StatusCode)
	}
	for _, path := range []string{"/sandboxes", "/v2/sandboxes"} {
		resp, data = doRequest(t, server, http.MethodGet, path, "", true)
		var list []sandboxDetail
		if err := json.Unmarshal(data, &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 105 || resp.Header.Get("X-Total-Running") != "105" || resp.Header.Get("X-Next-Token") != "" {
			t.Fatalf("Gateway fan-out list: size=%d headers=%v", len(list), resp.Header)
		}
	}
	query := url.Values{"template": {"base"}, "metadata": {"group=selected"}, "state": {"running"}}
	resp, data = doRequest(t, server, http.MethodGet, "/v2/sandboxes?"+query.Encode(), "", true)
	var filtered []sandboxDetail
	json.Unmarshal(data, &filtered)
	if len(filtered) != 53 || resp.Header.Get("X-Total-Running") != "53" {
		t.Fatalf("filtered list=%d headers=%v", len(filtered), resp.Header)
	}
	repeatedMetadata := url.Values{"metadata": {"group=other&group=selected"}, "state": {"running"}}
	resp, data = doRequest(t, server, http.MethodGet, "/v2/sandboxes?"+repeatedMetadata.Encode(), "", true)
	var repeatedFiltered []sandboxDetail
	if err := json.Unmarshal(data, &repeatedFiltered); err != nil {
		t.Fatal(err)
	}
	if len(repeatedFiltered) != 53 {
		t.Fatalf("repeated metadata filter=%d, want last value to win", len(repeatedFiltered))
	}
	for _, order := range []string{"asc", "desc"} {
		seen := make(map[string]bool)
		token := ""
		for pageIndex := 0; ; pageIndex++ {
			if pageIndex > 4 {
				t.Fatal("list cursor never terminated")
			}
			query := url.Values{"limit": {"40"}, "order": {order}}
			if token != "" {
				query.Set("nextToken", token)
			}
			resp, data := doRequest(t, server, http.MethodGet, "/v2/sandboxes?"+query.Encode(), "", true)
			var page []sandboxDetail
			if err := json.Unmarshal(data, &page); err != nil {
				t.Fatalf("page %d: %d %s", pageIndex, resp.StatusCode, data)
			}
			for _, item := range page {
				if len(item.SandboxID) != 36 || seen[item.SandboxID] {
					t.Fatalf("invalid or duplicate pagination ID %s", item.SandboxID)
				}
				seen[item.SandboxID] = true
			}
			token = resp.Header.Get("X-Next-Token")
			if token == "" {
				break
			}
		}
		if len(seen) != 105 {
			t.Errorf("%s pagination lost records: got %d", order, len(seen))
		}
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=abc", "state=creating", "order=sideways", "nextToken=invalid", "startedAfter=tomorrow", "metadata=%25bad%25", "broken=%xx"} {
		resp, _ := doRequest(t, server, http.MethodGet, "/v2/sandboxes?"+query, "", true)
		if resp.StatusCode != 400 {
			t.Errorf("invalid list query %q = HTTP %d", query, resp.StatusCode)
		}
	}
}

type coldTemplate struct{ conchtemplate.Store }

func (coldTemplate) Get(_ context.Context, name string) (conchtemplate.Entry, error) {
	if name != "base" {
		return conchtemplate.Entry{}, conchtemplate.ErrNotFound.New()
	}
	return conchtemplate.Entry{Name: "base", BootMode: conchtemplate.BootModeCold, BootIndexDigest: digest.FromString("preinstalled-template").String()}, nil
}

// fakeSandboxRuntime stands in for sandbox.Manager at the facade boundary. It
// models the Manager contract: Create returns the committed result, Delete
// returns nil only after a confirmed cleanup removed the record, and an error
// means the record was retained as UNKNOWN for an explicit delete or startup
// recovery.
type fakeSandboxRuntime struct {
	conchruntime.SandboxOps
	mu         sync.Mutex
	createReqs []sandbox.CreateRequest
	createErr  error
	deleted    []string
	deleteErr  error
	store      sandbox.Store
}

func (f *fakeSandboxRuntime) Create(ctx context.Context, req sandbox.CreateRequest) (runtimeapi.SandboxCreateResult, error) {
	f.mu.Lock()
	f.createReqs = append(f.createReqs, req)
	createErr := f.createErr
	f.mu.Unlock()
	if createErr != nil {
		return runtimeapi.SandboxCreateResult{}, createErr
	}
	createdAt := time.Now().UnixNano()
	if f.store != nil {
		if _, err := f.store.Create(ctx, sandbox.Record{
			ID: req.SandboxID, State: sandbox.StateReady, CreatedAt: createdAt,
			SourceTemplateName: req.TemplateName, SourceTemplateID: req.TemplateID,
			CheckpointHeadTemplateID: req.TemplateID, IP: "192.0.2.10",
			VCPUNum: req.VCPUNum, RamMB: req.RAMMB, E2B: req.E2B, EnvdVersion: "0.8.3",
			Metadata: req.Metadata, ExpiresAt: time.Now().Add(5 * time.Minute).UnixNano(),
		}); err != nil {
			return runtimeapi.SandboxCreateResult{}, err
		}
	}
	return runtimeapi.SandboxCreateResult{
		SandboxID: req.SandboxID, IP: "192.0.2.10", AgentToken: req.AgentToken,
		TemplateName: req.TemplateName, TemplateID: req.TemplateID,
		VCPUNum: req.VCPUNum, RamMB: req.RAMMB, CreatedAt: createdAt, EnvdVersion: "0.8.3",
	}, nil
}

func (f *fakeSandboxRuntime) Delete(ctx context.Context, sandboxID string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, sandboxID)
	deleteErr := f.deleteErr
	f.mu.Unlock()
	if deleteErr != nil {
		if f.store != nil {
			rec, err := f.store.Get(ctx, sandboxID)
			if err != nil {
				return err
			}
			rec.State = sandbox.StateUnknown
			rec.LastError = deleteErr.Error()
			if _, err := f.store.Update(ctx, rec); err != nil {
				return err
			}
		}
		return deleteErr
	}
	if f.store != nil {
		return f.store.Delete(ctx, sandboxID)
	}
	return nil
}

func (f *fakeSandboxRuntime) calls() (created int, deleted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.createReqs), append([]string(nil), f.deleted...)
}

func TestCreateAndKillContract(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	service.Templates = coldTemplate{}
	service.SetSandboxDefaults(conchruntime.SandboxDefaults{VCPUNum: 2, VCPUMax: 2, RamMB: 512})
	server := testServer(t, service, routes)
	body := `{"templateID":"base","secure":false,"timeout":60,"envVars":{"HELLO":"world"},"metadata":{"purpose":"sdk"}}`
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, data)
	}
	var created sandboxModel
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(created.SandboxID); err != nil || len(created.SandboxID) != 36 || created.EnvdVersion != "0.8.3" || resp.Header.Get(sandboxproxy.SandboxIDHeader) != created.SandboxID {
		t.Fatalf("invalid AgentENV create model: %s", data)
	}
	templateID := digest.FromString("preinstalled-template").String()
	if created.TemplateID != templateID {
		t.Fatalf("create model templateID = %q, want %q", created.TemplateID, templateID)
	}
	req := runtime.createReqs[0]
	if !req.E2B || req.Timeout != 60*time.Second || req.Env["HELLO"] != "world" || req.Metadata["purpose"] != "sdk" || req.TemplateName != "base" || req.TemplateID != templateID {
		t.Fatalf("runtime create request = %#v", req)
	}
	record, err := service.Store.Get(t.Context(), created.SandboxID)
	if err != nil || record.State != sandbox.StateReady || !record.E2B || record.Metadata["purpose"] != "sdk" || record.ExpiresAt <= time.Now().UnixNano() {
		t.Fatalf("created record = %+v error=%v", record, err)
	}
	// A digest templateID skips name resolution and reaches the runtime unnamed.
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes", `{"templateID":"`+templateID+`","secure":false}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("digest create = %d %s", resp.StatusCode, data)
	}
	if createdCalls, _ := runtime.calls(); createdCalls != 2 || runtime.createReqs[1].TemplateID != templateID || runtime.createReqs[1].TemplateName != "" {
		t.Fatalf("digest create request = %#v", runtime.createReqs[1])
	}
	// An unresolvable template name is a 404 that never reaches the runtime
	// and does not count as a node create failure.
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes", `{"templateID":"missing","secure":false}`, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown template create = %d %s", resp.StatusCode, data)
	}
	runtime.mu.Lock()
	runtime.createErr = sandbox.ErrResourceExhausted.New()
	runtime.mu.Unlock()
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-capacity create = %d", resp.StatusCode)
	}
	runtime.mu.Lock()
	runtime.createErr = nil
	runtime.mu.Unlock()
	successes, failures := service.CreateCounts()
	if successes != 2 || failures != 1 {
		t.Fatalf("create counts = %d/%d, want 2/1", successes, failures)
	}
	// Delete removes an E2B sandbox; native-only and unknown IDs never reach
	// the runtime.
	resp, data = doRequest(t, server, http.MethodDelete, "/sandboxes/"+created.SandboxID, "", true)
	if resp.StatusCode != http.StatusNoContent || len(data) != 0 {
		t.Fatalf("delete = %d %q", resp.StatusCode, data)
	}
	if _, err := service.Store.Get(t.Context(), created.SandboxID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("delete left record: %v", err)
	}
	private := seedRecord(t, service.Store, true, false, "selected")
	resp, _ = doRequest(t, server, http.MethodDelete, "/sandboxes/"+private.ID, "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("native delete = %d", resp.StatusCode)
	}
	resp, _ = doRequest(t, server, http.MethodDelete, "/sandboxes/"+uuid.NewString(), "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown delete = %d", resp.StatusCode)
	}
	if _, deleted := runtime.calls(); len(deleted) != 1 || deleted[0] != created.SandboxID {
		t.Fatalf("delete calls = %#v", deleted)
	}
	// An unconfirmed cleanup keeps the record as UNKNOWN; the facade surfaces
	// the error instead of reporting success.
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create before pending cleanup = %d %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.deleteErr = errors.New("VMM exit could not be confirmed")
	runtime.mu.Unlock()
	resp, _ = doRequest(t, server, http.MethodDelete, "/sandboxes/"+created.SandboxID, "", true)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("pending cleanup status = %d", resp.StatusCode)
	}
	pending, err := service.Store.Get(t.Context(), created.SandboxID)
	if err != nil || pending.State != sandbox.StateUnknown {
		t.Fatalf("pending record = %+v %v", pending, err)
	}
}

func TestRunExpiryDispatch(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)
	expired := seedRecord(t, service.Store, true, true, "expired")
	expired.ExpiresAt = time.Now().Add(-time.Second).UnixNano()
	if _, err := service.Store.Update(t.Context(), expired); err != nil {
		t.Fatal(err)
	}
	templateID := digest.FromString("preinstalled-template").String()
	unknown, err := service.Store.Create(t.Context(), sandbox.Record{
		ID: uuid.NewString(), State: sandbox.StateUnknown, SourceTemplateName: "base",
		SourceTemplateID: templateID, CheckpointHeadTemplateID: templateID, E2B: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	healthy := seedRecord(t, service.Store, true, true, "healthy")
	expiryCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); server.Config.Handler.(*Server).RunExpiry(expiryCtx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, deleted := runtime.calls()
		if len(deleted) == 1 {
			if deleted[0] != expired.ID {
				t.Fatalf("expiry dispatch: deleted=%v", deleted)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expiry dispatch did not converge: deleted=%v", deleted)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if _, err := service.Store.Get(t.Context(), expired.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("expired record survived: %v", err)
	}
	// An UNKNOWN record is never dispatched: it waits for an explicit delete.
	if _, err := service.Store.Get(t.Context(), unknown.ID); err != nil {
		t.Fatalf("unknown record was disturbed: %v", err)
	}
	if _, err := service.Store.Get(t.Context(), healthy.ID); err != nil {
		t.Fatalf("healthy record was disturbed: %v", err)
	}
	if _, deleted := runtime.calls(); len(deleted) != 1 {
		t.Fatalf("healthy sandbox dispatched: deleted=%v", deleted)
	}
}
