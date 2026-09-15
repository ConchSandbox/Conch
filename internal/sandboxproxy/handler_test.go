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
	"golang.org/x/net/websocket"
)

func proxyFor(t *testing.T, upstream http.Handler) (*httptest.Server, *Registry, string, string, uint64) {
	t.Helper()
	guest := httptest.NewServer(upstream)
	t.Cleanup(guest.Close)
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)
	registry := NewRegistry()
	id := uuid.NewString()
	gen := registry.Begin(id)
	if err := registry.Publish(id, gen, ip); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(NewHandler(registry, []string{"sandbox.example.test"}))
	t.Cleanup(proxy.Close)
	t.Cleanup(func() { registry.Remove(id, gen) })
	return proxy, registry, id, port, gen
}

func TestRoutingAndHeaderIsolation(t *testing.T) {
	requests := make(chan *http.Request, 10)
	proxy, _, id, port, _ := proxyFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	gen := registry.Begin(id)
	if err := registry.PublishWithHostMask(id, gen, ip, "app-${PORT}.internal"); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(NewHandler(registry, []string{"sandbox.example.test"}))
	defer proxy.Close()

	for _, test := range []struct{ name, host string }{
		{"host route", port + "-" + id + ".sandbox.example.test"},
		{"prefix route", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/app", nil)
			req.Header.Set(SandboxIDHeader, id)
			req.Header.Set(TargetPortHeader, port)
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

func TestProxyRejectsInvalidRoutes(t *testing.T) {
	proxy, _, id, _, _ := proxyFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestSSEFlushAndGenerationCancellation(t *testing.T) {
	canceled := make(chan struct{})
	proxy, registry, id, port, gen := proxyFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if !registry.Remove(id, gen) {
		t.Fatal("generation removal failed")
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("runtime invalidation did not cancel upstream SSE")
	}
}

func TestFullDuplexUpload(t *testing.T) {
	proxy, _, id, port, _ := proxyFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	proxy, registry, id, port, gen := proxyFor(t, websocket.Handler(func(ws *websocket.Conn) {
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
	registry.Remove(id, gen)
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
	proxy, _, id, port, _ := proxyFor(t, handler)
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
