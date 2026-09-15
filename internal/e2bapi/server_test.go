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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/api/go_proto/pbconnect"
	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdsandbox "github.com/openeuler/Conch/internal/adapters/containerd/sandbox"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/envd"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandboxproxy"
	conchtemplate "github.com/openeuler/Conch/internal/template"
	bolt "go.etcd.io/bbolt"
)

type runtimeapiCreateResult = runtimeapi.SandboxCreateResult

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
	service.ProxyRoutes = routes
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
	if err := routes.Publish(id, routes.Begin(id), ip); err != nil {
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
	for _, operation := range []string{"pause", "resume", "connect", "fork", "timeout", "refreshes"} {
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
	seedRecord(t, service.Store, false, true, "selected")
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

// localGuest stands in for the sandbox runtime: it answers E2B creates with a
// locally reachable guest and conch-init's process API for the envd version.
// deleteMode: 0 = clean delete, 1 = unconfirmed cleanup (Manager retains the
// record and returns an error), 2 = confirmed release (record deleted).
type localGuest struct {
	conchruntime.SandboxOps
	pbconnect.UnimplementedProcessServiceHandler
	mu         sync.Mutex
	token      string
	deleted    atomic.Int32
	initFailed atomic.Bool
	deleteMode atomic.Int32
	ip         string
	t          *testing.T
	store      sandbox.Store
}

func (g *localGuest) Create(ctx context.Context, req sandbox.CreateRequest) (runtimeapiCreateResult, error) {
	// Manager generates the agent token itself during create validation.
	if req.AgentToken == "" {
		token, err := sandbox.GenerateAgentToken()
		if err != nil {
			return runtimeapiCreateResult{}, err
		}
		req.AgentToken = token
	}
	g.mu.Lock()
	g.token = req.AgentToken
	g.mu.Unlock()
	result := runtimeapiCreateResult{SandboxID: req.SandboxID, IP: g.ip, AgentToken: req.AgentToken, TemplateID: req.TemplateID}
	if g.store != nil {
		if _, err := g.store.Create(ctx, sandbox.Record{
			ID: req.SandboxID, RuntimeID: req.RuntimeID, State: sandbox.StateReady,
			CreatedAt: time.Now().UnixNano(), SourceTemplateName: req.TemplateName, SourceTemplateID: req.TemplateID,
			CheckpointHeadTemplateID: req.TemplateID, IP: g.ip, VCPUNum: req.VCPUNum, RamMB: req.RAMMB,
		}); err != nil {
			return runtimeapiCreateResult{}, err
		}
	}
	return result, nil
}

func (g *localGuest) Delete(ctx context.Context, sandboxID string) error {
	g.deleted.Add(1)
	// myConch Manager.Delete expresses only two outcomes: nil (record deleted,
	// resources confirmed released) or an error (record retained as UNKNOWN,
	// reservation retained). Conch's third outcome — a diagnostic error after
	// a confirmed release — collapses into the clean one here.
	switch g.deleteMode.Load() {
	case 2:
		if g.store != nil {
			return g.store.Delete(ctx, sandboxID)
		}
		return nil
	case 1:
		if g.store != nil {
			rec, err := g.store.Get(ctx, sandboxID)
			if err != nil {
				return err
			}
			rec.State = sandbox.StateUnknown
			rec.LastError = "VMM exit could not be confirmed"
			if _, err := g.store.Update(ctx, rec); err != nil {
				return err
			}
		}
		return errors.New("VMM exit could not be confirmed")
	default:
		if g.store != nil {
			return g.store.Delete(ctx, sandboxID)
		}
		return nil
	}
}

func (g *localGuest) StartProcess(_ context.Context, req *connect.Request[pb.StartProcessRequest], stream *connect.ServerStream[pb.ProcessEvent]) error {
	g.mu.Lock()
	token := g.token
	g.mu.Unlock()
	if req.Header().Get("conch-init-token") != token || req.Msg.Cmd != "/usr/bin/envd" || len(req.Msg.Args) != 1 || req.Msg.Args[0] != "-version" {
		return connect.NewError(connect.CodePermissionDenied, errors.New("invalid guest command"))
	}
	if err := stream.Send(&pb.ProcessEvent{Event: &pb.ProcessEvent_Data{Data: &pb.ProcessDataEvent{Output: &pb.ProcessDataEvent_Stdout{Stdout: []byte("0.8.3\n")}}}}); err != nil {
		return err
	}
	return stream.Send(&pb.ProcessEvent{Event: &pb.ProcessEvent_End{End: &pb.ProcessEndEvent{Exited: true}}})
}

func listenGuest(t *testing.T, ip string, port int, handler http.Handler) {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
}

func TestCreateWaitsForEnvdAndInitializesThenDelete(t *testing.T) {
	service, routes := testRuntime(t)
	guest := &localGuest{ip: "127.0.0.38", t: t, store: service.Store}
	service.Sandbox = guest
	service.Templates = coldTemplate{}
	service.SetSandboxDefaults(conchruntime.SandboxDefaults{VCPUNum: 2, VCPUMax: 2, RamMB: 512})
	service.Envd = envd.NewClient()
	var err error
	service.Capacity, err = conchruntime.NewCapacity(conchruntime.CapacityLimits{MaxSandboxes: 1, MaxCPUs: 2, MaxMemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	_, process := pbconnect.NewProcessServiceHandler(guest)
	listenGuest(t, guest.ip, 4064, process)
	initialized := atomic.Bool{}
	listenGuest(t, guest.ip, envd.DefaultPort, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/init" {
			if guest.initFailed.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request["defaultUser"] != "user" || request["defaultWorkdir"] != "/home/user" || request["envVars"].(map[string]any)["HELLO"] != "world" {
				t.Errorf("wrong envd init payload %#v", request)
			}
			initialized.Store(true)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server := testServer(t, service, routes)
	body := `{"templateID":"base","secure":false,"timeout":60,"envVars":{"HELLO":"world"},"metadata":{"purpose":"sdk"}}`
	// A real failed /init must roll back VM, store, route and capacity so a
	// subsequent create can consume the single available slot.
	guest.initFailed.Store(true)
	resp, _ := doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusInternalServerError || guest.deleted.Load() != 1 {
		t.Fatalf("failed bootstrap cleanup: HTTP %d deleted=%d", resp.StatusCode, guest.deleted.Load())
	}
	remaining, _ := service.Store.List(t.Context(), sandbox.Filter{})
	if len(remaining) != 0 {
		t.Fatal("failed bootstrap left persisted sandbox")
	}
	guest.initFailed.Store(false)
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusCreated || !initialized.Load() {
		t.Fatalf("create = %d %q initialized=%v", resp.StatusCode, data, initialized.Load())
	}
	var created sandboxModel
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(created.SandboxID); err != nil || len(created.SandboxID) != 36 || created.EnvdVersion != "0.8.3" || resp.Header.Get(sandboxproxy.SandboxIDHeader) != created.SandboxID {
		t.Fatalf("invalid AgentENV create model: %s", data)
	}
	if _, ok := routes.Lookup(created.SandboxID); !ok {
		t.Fatal("201 returned without a published route")
	}
	record, err := service.Store.Get(t.Context(), created.SandboxID)
	if err != nil || record.State != sandbox.StateReady || record.Metadata["purpose"] != "sdk" || record.ExpiresAt <= time.Now().UnixNano() {
		t.Fatalf("created record = %+v error=%v", record, err)
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("over-capacity create = %d", resp.StatusCode)
	}
	resp, data = doRequest(t, server, http.MethodDelete, "/sandboxes/"+created.SandboxID, "", true)
	if resp.StatusCode != http.StatusNoContent || len(data) != 0 || guest.deleted.Load() != 2 {
		t.Fatalf("delete = %d %q deleted=%d", resp.StatusCode, data, guest.deleted.Load())
	}
	if _, ok := routes.Lookup(created.SandboxID); ok {
		t.Fatal("delete left active proxy route")
	}
	if _, err := service.Store.Get(t.Context(), created.SandboxID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("delete left record: %v", err)
	}
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("delete did not free capacity: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	doRequest(t, server, http.MethodDelete, "/sandboxes/"+created.SandboxID, "", true)
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes", strings.Replace(body, `"timeout":60`, `"timeout":0`, 1), true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("zero TTL create: %d %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	record, err = service.GetSandbox(t.Context(), created.SandboxID)
	if err != nil || record.ExpiresAt <= 0 || record.ExpiresAt > time.Now().UnixNano() {
		t.Fatalf("zero TTL must be due immediately: %+v %v", record, err)
	}
	expiryCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); server.Config.Handler.(*Server).RunExpiry(expiryCtx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := service.GetSandbox(t.Context(), created.SandboxID)
		if errors.Is(err, sandbox.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("zero TTL sandbox was not deleted: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := routes.Lookup(created.SandboxID); ok {
		t.Fatal("expiry left a proxy route")
	}
	// A confirmed release removes the record even when Manager surfaces a
	// diagnostic; an unconfirmed cleanup keeps a retryable UNKNOWN owner that
	// expiry reaps once confirmed.
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create before diagnostic: %d %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	guest.deleteMode.Store(2)
	resp, _ = doRequest(t, server, http.MethodDelete, "/sandboxes/"+created.SandboxID, "", true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("confirmed cleanup status=%d", resp.StatusCode)
	}
	if _, err := service.GetSandbox(t.Context(), created.SandboxID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("confirmed cleanup retained record: %v", err)
	}
	guest.deleteMode.Store(0)
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("confirmed cleanup stranded capacity: %d %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	guest.deleteMode.Store(1)
	resp, _ = doRequest(t, server, http.MethodDelete, "/sandboxes/"+created.SandboxID, "", true)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("pending cleanup status=%d", resp.StatusCode)
	}
	pending, err := service.GetSandbox(t.Context(), created.SandboxID)
	if err != nil || pending.State != sandbox.StateUnknown {
		t.Fatalf("pending owner=%+v %v", pending, err)
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("unconfirmed capacity reused: %d", resp.StatusCode)
	}
	guest.deleteMode.Store(0)
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, err := service.GetSandbox(t.Context(), created.SandboxID)
		if errors.Is(err, sandbox.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("maintenance did not retry pending cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("cleanup retry stranded capacity: %d %s", resp.StatusCode, data)
	}
}
