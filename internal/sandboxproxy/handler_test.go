package sandboxproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/api/go_proto/pbconnect"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/pkg/requestid"
	"golang.org/x/net/websocket"
)

func proxyFor(t *testing.T, upstream http.Handler) (*httptest.Server, *Registry, string, string) {
	t.Helper()
	guest := httptest.NewServer(upstream)
	t.Cleanup(guest.Close)
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)
	registry := NewRegistry()
	id := uuid.NewString()
	if r := registry.Insert(id); r == nil {
		t.Fatal("insert failed")
	}
	if err := registry.Publish(id, ip, ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(NewHandler(registry, []string{"sandbox.example.test"}, nil))
	t.Cleanup(proxy.Close)
	t.Cleanup(func() { registry.Release(id) })
	return proxy, registry, id, port
}

func TestRoutingAndHeaderIsolation(t *testing.T) {
	requests := make(chan *http.Request, 10)
	proxy, _, id, port := proxyFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		// Write the actual HTTP response so the Go server does not replace
		// Connection with "close" for the proxy's unpooled request.
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		body := "application response"
		fmt.Fprintf(rw, "HTTP/1.1 201 Created\r\nConnection: X-Private-Hop\r\nX-Private-Hop: hidden\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		rw.Flush()
	}))
	for _, test := range []struct {
		name, path, expectedPath, host string
		alias                          bool
	}{
		{"prefix", "/proxy/files/a%2Fb?path=%2Ftmp%2Fa+b", "/files/a%2Fb?path=%2Ftmp%2Fa+b", "", false},
		{"escaped prefix", "/pr%6fxy/files/a%2Fb", "/files/a%2Fb", "", false},
		{"bare prefix", "/proxy", "/", "", false},
		{"fallback alias", "/files/a%2Fb?hello=world", "/files/a%2Fb?hello=world", "", true},
		{"host takes precedence", "/sandboxes", "/sandboxes", port + "-" + id + ".sandbox.example.test", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, proxy.URL+test.path, nil)
			req.Header.Set(SandboxIDHeader, id)
			req.Header.Set(TargetPortHeader, port)
			if test.alias {
				req.Header.Del(SandboxIDHeader)
				req.Header.Del(TargetPortHeader)
				req.Header.Set(E2BSandboxIDHeader, id)
				req.Header.Set(E2BTargetPortHeader, port)
			}
			if test.host != "" {
				req.Host = test.host
				req.Header.Set(SandboxIDHeader, uuid.NewString())
				req.Header.Set(TargetPortHeader, "1")
			}
			for _, header := range []string{"X-API-Key", "X-Access-Token", "E2b-Traffic-Access-Token", "Conch-Init-Token"} {
				req.Header.Set(header, "must-not-reach-guest")
			}
			req.Header.Set("Authorization", "Bearer application-owned")
			req.Header.Set("Connect-Protocol-Version", "1")
			req.Header.Set("Connection", "X-Private-Request")
			req.Header.Set("X-Private-Request", "hidden")
			resp, err := proxy.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusCreated || string(body) != "application response" {
				t.Fatalf("response = %d %q", resp.StatusCode, body)
			}
			if resp.Header.Get("X-Private-Hop") != "" {
				t.Fatal("response leaked hop-by-hop header")
			}
			got := <-requests
			if got.URL.RequestURI() != test.expectedPath {
				t.Errorf("forwarded URI = %q, want %q", got.URL.RequestURI(), test.expectedPath)
			}
			for _, header := range []string{SandboxIDHeader, E2BSandboxIDHeader, TargetPortHeader, E2BTargetPortHeader, "X-API-Key", "X-Access-Token", "E2b-Traffic-Access-Token", "Conch-Init-Token", "X-Private-Request"} {
				if got.Header.Get(header) != "" {
					t.Errorf("guest received private header %s", header)
				}
			}
			if got.Header.Get("Authorization") != "Bearer application-owned" || got.Header.Get("Connect-Protocol-Version") != "1" {
				t.Error("proxy lost application or Connect headers")
			}
		})
	}
}

// TestHostMaskRewrite proves maskRequestHost hides the sandbox domain: the
// upstream sees the masked Host with ${PORT} substituted, and the original
// host is preserved in X-Forwarded-Host.
func TestHostMaskRewrite(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s", r.Host, r.Header.Get("X-Forwarded-Host"))
	}))
	defer guest.Close()
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)
	registry := NewRegistry()
	defer registry.Close()
	id := uuid.NewString()
	if registry.Insert(id) == nil {
		t.Fatal("insert failed")
	}
	if err := registry.Publish(id, ip, "app-${PORT}.internal"); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(NewHandler(registry, []string{"sandbox.example.test"}, nil))
	defer proxy.Close()

	for _, test := range []struct{ name, host string }{
		{"host route", port + "-" + id + ".sandbox.example.test"},
		{"prefix route", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/app", nil)
			req.Header.Set(SandboxIDHeader, id)
			req.Header.Set(TargetPortHeader, port)
			// A client-supplied X-Forwarded-Host is untrusted input: it must
			// not chain into the value the guest recovers.
			req.Header.Set("X-Forwarded-Host", "client-supplied.example")
			if test.host != "" {
				req.Host = test.host
				req.Header.Set(SandboxIDHeader, uuid.NewString())
				req.Header.Set(TargetPortHeader, "1")
			}
			resp, err := proxy.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			want := "app-" + port + ".internal|" + test.host
			if test.host == "" {
				want = "app-" + port + ".internal|" + req.Host
			}
			if string(body) != want {
				t.Fatalf("upstream saw %q, want %q", body, want)
			}
		})
	}
}

// TestHostPreservedWithoutMask proves an unmasked host route forwards the
// inbound Host unchanged: the upstream sees the caller's domain, and
// X-Forwarded-Host carries the same value.
func TestHostPreservedWithoutMask(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s", r.Host, r.Header.Get("X-Forwarded-Host"))
	}))
	defer guest.Close()
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)
	registry := NewRegistry()
	defer registry.Close()
	id := uuid.NewString()
	if registry.Insert(id) == nil {
		t.Fatal("insert failed")
	}
	if err := registry.Publish(id, ip, ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(NewHandler(registry, []string{"sandbox.example.test"}, nil))
	defer proxy.Close()

	host := port + "-" + id + ".sandbox.example.test"
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/app", nil)
	req.Host = host
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if want := host + "|" + host; string(body) != want {
		t.Fatalf("upstream saw %q, want %q", body, want)
	}
}

// TestWakeServesRestoredRoute proves a miss on a wakeable sandbox restores its
// route inside the wake callback and then serves the request, while later
// requests use the published route without waking again.
func TestWakeServesRestoredRoute(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "woken guest")
	}))
	defer guest.Close()
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)
	registry := NewRegistry()
	defer registry.Close()
	id := uuid.NewString()
	wakes := 0
	wake := func(_ context.Context, sandboxID string) error {
		wakes++
		if sandboxID != id {
			return sandbox.ErrNotFound.New()
		}
		if registry.Insert(id) == nil {
			return sandbox.ErrFailedPrecondition.New()
		}
		return registry.Publish(id, ip, "")
	}
	proxy := httptest.NewServer(NewHandler(registry, nil, wake))
	defer proxy.Close()

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/proxy/health", nil)
		req.Header.Set(SandboxIDHeader, id)
		req.Header.Set(TargetPortHeader, port)
		resp, err := proxy.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "woken guest" {
			t.Fatalf("request %d = %d %q", i, resp.StatusCode, body)
		}
	}
	if wakes != 1 {
		t.Fatalf("wakes = %d, want 1", wakes)
	}
}

// TestWakeErrorClassification covers the miss-path status mapping for wake
// failures and for a wake that succeeds without producing a route.
func TestWakeErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wake   func(context.Context, string) error
		status int
	}{
		{"not found", func(context.Context, string) error { return sandbox.ErrNotFound.New() }, http.StatusNotFound},
		{"not resumable", func(context.Context, string) error { return sandbox.ErrFailedPrecondition.New() }, http.StatusServiceUnavailable},
		{"wake deadline", func(ctx context.Context, _ string) error { return context.DeadlineExceeded }, http.StatusGatewayTimeout},
		{"no route appears", func(context.Context, string) error { return nil }, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewRegistry()
			defer registry.Close()
			proxy := httptest.NewServer(NewHandler(registry, nil, tc.wake))
			defer proxy.Close()
			req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/proxy/health", nil)
			req.Header.Set(SandboxIDHeader, uuid.NewString())
			req.Header.Set(TargetPortHeader, "80")
			resp, err := proxy.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != tc.status {
				t.Fatalf("wake = %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

// TestWakeOutlivesWakerDisconnect proves the restore runs on its own budget:
// the request that triggered the wake disconnects mid-restore, and a waiter
// that arrived meanwhile is still served by the route the wake publishes.
func TestWakeOutlivesWakerDisconnect(t *testing.T) {
	oldWait := bootstrapWait
	bootstrapWait = 400 * time.Millisecond
	defer func() { bootstrapWait = oldWait }()

	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Guest-Request-ID", r.Header.Get(requestid.Header))
		fmt.Fprint(w, "woken guest")
	}))
	defer guest.Close()
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)
	registry := NewRegistry()
	defer registry.Close()
	id := uuid.NewString()
	wakeStarted := make(chan struct{})
	wakerGone := make(chan struct{})
	wakes := 0
	wake := func(ctx context.Context, sandboxID string) error {
		wakes++
		if registry.Insert(sandboxID) == nil {
			return sandbox.ErrFailedPrecondition.New()
		}
		close(wakeStarted)
		<-wakerGone
		// The restore aborts with its context, as the create kernel would.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if got := requestid.FromContext(ctx); got != "wake-request-206" {
			t.Errorf("wake lost source ID after disconnect: %q", got)
		}
		return registry.Publish(sandboxID, ip, "")
	}
	handler := requestid.Middleware(NewHandler(registry, nil, wake))

	wakerCtx, cancelWaker := context.WithCancel(t.Context())
	defer cancelWaker()
	waker, _ := http.NewRequestWithContext(wakerCtx, http.MethodGet, "http://proxy/proxy/health", nil)
	waker.Header.Set(SandboxIDHeader, id)
	waker.Header.Set(TargetPortHeader, port)
	waker.Header.Set(requestid.Header, "wake-request-206")
	wakerDone := make(chan struct{})
	go func() {
		defer close(wakerDone)
		handler.ServeHTTP(httptest.NewRecorder(), waker)
	}()
	<-wakeStarted

	waiterDone := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		waiter, _ := http.NewRequest(http.MethodGet, "http://proxy/proxy/health", nil)
		waiter.Header.Set(SandboxIDHeader, id)
		waiter.Header.Set(TargetPortHeader, port)
		waiter.Header.Set(requestid.Header, "waiter-request-206")
		handler.ServeHTTP(rec, waiter)
		waiterDone <- rec
	}()

	cancelWaker()
	close(wakerGone)
	select {
	case rec := <-waiterDone:
		if rec.Code != http.StatusOK || rec.Body.String() != "woken guest" {
			t.Fatalf("waiter after waker disconnect = %d %q", rec.Code, rec.Body.String())
		}
		if rec.Header().Get(requestid.Header) != "waiter-request-206" || rec.Header().Get("X-Guest-Request-ID") != "waiter-request-206" {
			t.Fatalf("waiter inherited wake identity: %v", rec.Header())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never served after the waker disconnected")
	}
	<-wakerDone
	if wakes != 1 {
		t.Fatalf("wakes = %d, want 1", wakes)
	}
}

// TestBootstrapWaitServesRouteWithoutWake proves a request arriving while a
// bootstrap slot exists (route not yet published) waits for the route instead
// of waking the sandbox again.
func TestBootstrapWaitServesRouteWithoutWake(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "bootstrapped guest")
	}))
	defer guest.Close()
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)
	registry := NewRegistry()
	defer registry.Close()
	id := uuid.NewString()
	if registry.Insert(id) == nil {
		t.Fatal("insert failed")
	}
	published := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := registry.Publish(id, ip, ""); err != nil {
			t.Error(err)
		}
		close(published)
	}()
	proxy := httptest.NewServer(NewHandler(registry, nil, func(context.Context, string) error {
		t.Error("bootstrap wait must not call wake")
		return sandbox.ErrFailedPrecondition.New()
	}))
	defer proxy.Close()
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/proxy/health", nil)
	req.Header.Set(SandboxIDHeader, id)
	req.Header.Set(TargetPortHeader, port)
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	<-published
	if resp.StatusCode != http.StatusOK || string(body) != "bootstrapped guest" {
		t.Fatalf("bootstrap wait = %d %q", resp.StatusCode, body)
	}
}

func TestProxyRejectsInvalidRoutes(t *testing.T) {
	proxy, _, id, _ := proxyFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid routing reached upstream")
	}))
	for _, port := range []string{"", "0", "65536", "-1", "80/path", "127.0.0.1:80"} {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/proxy/health", nil)
		req.Header.Set(SandboxIDHeader, id)
		req.Header.Set(TargetPortHeader, port)
		resp, err := proxy.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("port %q: HTTP %d", port, resp.StatusCode)
		}
	}
}

// TestAwaitRoute exercises the bootstrap wait: an unpublished slot makes the
// request wait, a slot published while waiting serves the request through the
// proxy, a slot released while waiting answers 502, an exhausted budget
// answers 504, a canceled request answers 410, and a sandbox without a slot
// answers 404.
func TestAwaitRoute(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "booted")
	}))
	defer guest.Close()
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)

	publishAfter := func(delay time.Duration, publish func(*Registry, string)) func(*Registry, string) {
		return func(r *Registry, id string) {
			time.Sleep(delay)
			publish(r, id)
		}
	}

	for _, test := range []struct {
		name     string
		slot     bool
		disturb  func(*Registry, string)
		cancelAt time.Duration
		want     int
	}{
		{name: "waits for publication", slot: true, disturb: publishAfter(150*time.Millisecond, func(r *Registry, id string) {
			if err := r.Publish(id, ip, ""); err != nil {
				t.Error(err)
			}
		}), want: http.StatusOK},
		{name: "released slot answers 502", slot: true, disturb: publishAfter(150*time.Millisecond, func(r *Registry, id string) {
			r.Release(id)
		}), want: http.StatusBadGateway},
		{name: "exhausted budget answers 504", slot: true, want: http.StatusGatewayTimeout},
		{name: "canceled request answers 410", slot: true, cancelAt: 150 * time.Millisecond, want: http.StatusGone},
		{name: "missing slot answers 404", want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldWait := bootstrapWait
			bootstrapWait = 400 * time.Millisecond
			defer func() { bootstrapWait = oldWait }()
			registry := NewRegistry()
			defer registry.Close()
			id := uuid.NewString()
			if test.slot && registry.Insert(id) == nil {
				t.Fatal("insert failed")
			}
			if test.disturb != nil {
				go test.disturb(registry, id)
			}
			handler := NewHandler(registry, nil, nil)
			ctx := t.Context()
			var cancel context.CancelFunc
			if test.cancelAt > 0 {
				ctx, cancel = context.WithCancel(ctx)
				time.AfterFunc(test.cancelAt, cancel)
				defer cancel()
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://proxy/proxy/health", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(SandboxIDHeader, id)
			req.Header.Set(TargetPortHeader, port)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != test.want {
				t.Fatalf("HTTP %d, want %d", w.Code, test.want)
			}
			if test.want == http.StatusOK && w.Body.String() != "booted" {
				t.Fatalf("body = %q, want %q", w.Body.String(), "booted")
			}
		})
	}
}

func TestSSEFlushAndRouteCancellation(t *testing.T) {
	canceled := make(chan struct{})
	proxy, registry, id, port := proxyFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ready\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/events", nil)
	req.Header.Set(E2BSandboxIDHeader, id)
	req.Header.Set(E2BTargetPortHeader, port)
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: ready\n" {
		t.Fatalf("stream first message = %q, %v", line, err)
	}
	registry.Release(id)
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("runtime invalidation did not cancel upstream SSE")
	}
}

func TestFullDuplexUpload(t *testing.T) {
	proxy, _, id, port := proxyFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		scanner := bufio.NewScanner(r.Body)
		for scanner.Scan() {
			fmt.Fprintln(w, scanner.Text())
			w.(http.Flusher).Flush()
		}
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/process.Process/StreamInput", reader)
	req.Header.Set(SandboxIDHeader, id)
	req.Header.Set(TargetPortHeader, port)
	go func() { fmt.Fprintln(writer, "first") }()
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	if !scanner.Scan() || scanner.Text() != "first" {
		t.Fatalf("first response before upload EOF: %s, %v", scanner.Text(), scanner.Err())
	}
	go func() { fmt.Fprintln(writer, "second"); writer.Close() }()
	if !scanner.Scan() || scanner.Text() != "second" {
		t.Fatalf("second streamed response: %s, %v", scanner.Text(), scanner.Err())
	}
}

func TestWebSocketEchoAndInvalidation(t *testing.T) {
	proxy, registry, id, port := proxyFor(t, websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		for {
			var data string
			if err := websocket.Message.Receive(ws, &data); err != nil {
				return
			}
			if err := websocket.Message.Send(ws, data); err != nil {
				return
			}
		}
	}))
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(proxy.URL, "http")+"/proxy/pty", proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.Header.Set(SandboxIDHeader, id)
	config.Header.Set(TargetPortHeader, port)
	ws, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetDeadline(time.Now().Add(3 * time.Second))
	if err := websocket.Message.Send(ws, "terminal bytes"); err != nil {
		t.Fatal(err)
	}
	var data string
	if err := websocket.Message.Receive(ws, &data); err != nil || data != "terminal bytes" {
		t.Fatalf("WebSocket echo = %q, %v", data, err)
	}
	registry.Release(id)
	if err := websocket.Message.Receive(ws, &data); err == nil {
		t.Fatal("WebSocket remained usable after runtime removal")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("runtime removal did not close WebSocket")
	}
}

type streamingProcess struct {
	pbconnect.UnimplementedProcessServiceHandler
}

func (streamingProcess) StartProcess(_ context.Context, _ *connect.Request[pb.StartProcessRequest], stream *connect.ServerStream[pb.ProcessEvent]) error {
	for _, output := range []string{"first", "second"} {
		if err := stream.Send(&pb.ProcessEvent{Event: &pb.ProcessEvent_Data{Data: &pb.ProcessDataEvent{Output: &pb.ProcessDataEvent_Stdout{Stdout: []byte(output)}}}}); err != nil {
			return err
		}
	}
	return stream.Send(&pb.ProcessEvent{Event: &pb.ProcessEvent_End{End: &pb.ProcessEndEvent{Exited: true}}})
}

func TestConnectServerStream(t *testing.T) {
	_, handler := pbconnect.NewProcessServiceHandler(streamingProcess{})
	proxy, _, id, port := proxyFor(t, handler)
	client := pbconnect.NewProcessServiceClient(proxy.Client(), proxy.URL)
	req := connect.NewRequest(&pb.StartProcessRequest{Cmd: "echo"})
	req.Header().Set(E2BSandboxIDHeader, id)
	req.Header().Set(E2BTargetPortHeader, port)
	stream, err := client.StartProcess(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var output strings.Builder
	ended := false
	for stream.Receive() {
		output.Write(stream.Msg().GetData().GetStdout())
		ended = ended || stream.Msg().GetEnd() != nil
	}
	if err := stream.Err(); err != nil || output.String() != "firstsecond" || !ended {
		t.Fatalf("Connect output=%q ended=%v error=%v", output.String(), ended, err)
	}
}
