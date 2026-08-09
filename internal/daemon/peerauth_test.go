package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testIdentity(uid, gid uint32) peerIdentity {
	return peerIdentity{PID: 4242, UID: uid, GID: gid}
}

// The whole policy: root in, everyone else out. conchd runs as root, so this
// repeats what the socket's own 0660 root:root mode already enforces.
func TestAuthorizePeerAdmitsRootOnly(t *testing.T) {
	t.Parallel()
	if err := authorizePeer(testIdentity(0, 0)); err != nil {
		t.Fatalf("root denied: %v", err)
	}
	for _, id := range []peerIdentity{
		testIdentity(1000, 1000),
		testIdentity(65534, 65534),
		// A non-root uid whose primary group is root's.
		testIdentity(1000, 0),
	} {
		if err := authorizePeer(id); !errors.Is(err, errPeerNotAuthorized) {
			t.Errorf("authorize(%s) = %v, want errPeerNotAuthorized", id, err)
		}
	}
}

// TestMiddlewareDeniesRequestWithoutPeerIdentity is the fail-closed assertion: a
// request that carries no credentials must not reach a handler.
func TestMiddlewareDeniesRequestWithoutPeerIdentity(t *testing.T) {
	t.Parallel()
	reached := false
	handler := peerAuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/image/pull", nil))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if reached {
		t.Fatal("handler ran for a request with no peer credentials")
	}
}

func TestMiddlewareAllowsHealthWithoutPeerIdentity(t *testing.T) {
	t.Parallel()
	reached := false
	handler := peerAuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, healthPath, nil))

	if !reached {
		t.Fatalf("health probe was blocked: status = %d", rec.Code)
	}
}

// controlPlanePatterns mirrors the routes registered by (*Daemon).routes, minus
// /health. ServeMux cannot be asked what it holds, so this is a copy: adding an
// endpoint needs a line here. The middleware wraps the whole mux, so a new
// endpoint is guarded either way; only this assertion needs the update.
var controlPlanePatterns = []string{
	"GET /api/v1/sandboxes",
	"POST /api/v1/sandboxes",
	"GET /api/v1/sandboxes/{sandboxID}",
	"DELETE /api/v1/sandboxes/{sandboxID}",
	"/api/sandbox/suspend",
	"/api/sandbox/resume",
	"/api/sandbox/checkpoint",
	"/api/template/create",
	"/api/template/pull",
	"/api/template/push",
	"/api/template/list",
	"/api/template/inspect",
	"/api/template/remove",
	"/api/snapshot/list",
	"/api/snapshot/remove",
	"/api/snapshot/info",
	"/api/image/pull",
	"/api/image/push",
	"/api/image/list",
	"/api/image/remove",
	"/api/image/unpack",
	"/api/image/import",
}

// requestForPattern turns a ServeMux pattern into a concrete request.
func requestForPattern(pattern string) *http.Request {
	method := http.MethodPost
	path := pattern
	if verb, rest, found := strings.Cut(pattern, " "); found {
		method = verb
		path = rest
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") {
			segments[i] = "sample-id"
		}
	}
	return httptest.NewRequest(method, strings.Join(segments, "/"), nil)
}

// The finding in issue #95 was that every endpoint answered unauthenticated;
// this sweeps all of them.
func TestMiddlewareGuardsEveryControlPlaneEndpoint(t *testing.T) {
	t.Parallel()
	daemon := &Daemon{router: http.NewServeMux()}
	daemon.routes()

	handler := peerAuthMiddleware(daemon.router)

	guarded := 0
	for _, pattern := range controlPlanePatterns {
		req := requestForPattern(pattern)
		ctx := context.WithValue(req.Context(), peerIdentityKey{}, testIdentity(1000, 1000))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req.WithContext(ctx))

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want %d", pattern, rec.Code, http.StatusForbidden)
			continue
		}
		guarded++
	}

	if guarded != len(controlPlanePatterns) {
		t.Fatalf("guarded %d endpoints, want %d", guarded, len(controlPlanePatterns))
	}
	if guarded < 21 {
		t.Fatalf("guarded %d endpoints, but issue #95 reported 21 unauthenticated endpoints", guarded)
	}
}

// Counterweight to the refusals: a middleware that blocked everything would
// satisfy all of them.
func TestMiddlewareAdmitsRoot(t *testing.T) {
	t.Parallel()
	reached := false
	handler := peerAuthMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	req := httptest.NewRequest(http.MethodPost, "/api/image/pull", nil)
	ctx := context.WithValue(req.Context(), peerIdentityKey{}, testIdentity(0, 0))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req.WithContext(ctx))

	if !reached {
		t.Fatalf("root was blocked: status = %d", rec.Code)
	}
}

// The real SO_PEERCRED path; everything above uses synthesized identities.
func TestPeerCredentialReportsConnectingProcess(t *testing.T) {
	t.Parallel()
	path := socketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type result struct {
		identity peerIdentity
		err      error
	}
	results := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			results <- result{err: err}
			return
		}
		defer conn.Close()
		identity, err := peerCredential(conn.(*net.UnixConn))
		results <- result{identity: identity, err: err}
	}()

	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	got := <-results
	if got.err != nil {
		t.Fatalf("peerCredential: %v", got.err)
	}
	if got.identity.UID != uint32(os.Getuid()) {
		t.Errorf("uid = %d, want %d", got.identity.UID, os.Getuid())
	}
	if got.identity.GID != uint32(os.Getgid()) {
		t.Errorf("gid = %d, want %d", got.identity.GID, os.Getgid())
	}
	if got.identity.PID != int32(os.Getpid()) {
		t.Errorf("pid = %d, want %d", got.identity.PID, os.Getpid())
	}
}

// End to end: a real request over a real socket, through the same ConnContext
// wiring Start uses. The outcome follows the uid the suite runs as, so it
// covers admission under root and refusal under any other account.
func TestUnixSocketRequestCarriesPeerIdentity(t *testing.T) {
	t.Parallel()
	wantStatus := http.StatusForbidden
	if os.Geteuid() == 0 {
		wantStatus = http.StatusOK
	}

	path := socketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{
		Handler: peerAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})),
		ConnContext: peerIdentityConnContext,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Post("http://conchd-unix/api/image/pull", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d (running as uid %d)", resp.StatusCode, wantStatus, os.Geteuid())
	}
}
