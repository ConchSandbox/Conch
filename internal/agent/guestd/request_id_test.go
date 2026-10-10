package guestd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/openeuler/Conch/api/go_proto"
	agentconnect "github.com/openeuler/Conch/api/go_proto/pbconnect"
	"github.com/openeuler/Conch/pkg/requestid"
	"github.com/openeuler/Conch/pkg/ulog"
)

func TestAgentRequestIDOnUnaryStreamingAndAuthErrors(t *testing.T) {
	resetAgentAuth(t)
	agentAuth.SetToken("secret")
	previous := ulog.GetLogger()
	t.Cleanup(func() { ulog.SetLogger(previous) })
	logPath := filepath.Join(t.TempDir(), "agent.log")
	if err := ulog.Init(ulog.Config{Level: ulog.InfoLevel, OutputFile: logPath}); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewUnstartedServer(newAgentHTTPHandler())
	h.EnableHTTP2 = true
	h.StartTLS()
	defer h.Close()
	client := agentconnect.NewProcessServiceClient(h.Client(), h.URL)
	for _, authorized := range []bool{false, true} {
		req := connect.NewRequest(&pb.ListProcessesRequest{})
		req.Header().Set(requestid.Header, "agent-unary-206")
		if authorized {
			req.Header().Set(agentTokenHeaderKey, "secret")
		}
		resp, err := client.List(context.Background(), req)
		if authorized {
			if err != nil || resp.Header().Get(requestid.Header) != "agent-unary-206" {
				t.Fatalf("List: %v, response %v", err, resp)
			}
		} else {
			connectErr, ok := err.(*connect.Error)
			if !ok || connectErr.Code() != connect.CodeUnauthenticated || connectErr.Meta().Get(requestid.Header) != "agent-unary-206" {
				t.Fatalf("List auth error: %v", err)
			}
		}
	}
	req := connect.NewRequest(&pb.StartProcessRequest{Cmd: "sh", Args: []string{"-c", "echo correlated"}, Cwd: t.TempDir()})
	req.Header().Set(agentTokenHeaderKey, "secret")
	req.Header().Set(requestid.Header, "agent-stream-206")
	stream, err := client.StartProcess(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if stream.ResponseHeader().Get(requestid.Header) != "agent-stream-206" {
		t.Fatal("stream response lost request ID")
	}
	fileClient := agentconnect.NewFileServiceClient(h.Client(), h.URL)
	path := filepath.Join(t.TempDir(), "correlated.txt")
	upload := fileClient.PostFileStream(context.Background())
	upload.RequestHeader().Set(agentTokenHeaderKey, "secret")
	upload.RequestHeader().Set(requestid.Header, "agent-upload-206")
	if err := upload.Send(&pb.FileChunk{Filepath: path, Content: []byte("correlated")}); err != nil {
		t.Fatal(err)
	}
	resp, err := upload.CloseAndReceive()
	if err != nil || resp.Header().Get(requestid.Header) != "agent-upload-206" {
		t.Fatalf("upload: %v, response %v", err, resp)
	}
	downloadReq := connect.NewRequest(&pb.GetFileRequest{Filepath: path})
	downloadReq.Header().Set(agentTokenHeaderKey, "secret")
	downloadReq.Header().Set(requestid.Header, "agent-download-206")
	download, err := fileClient.GetFileStream(context.Background(), downloadReq)
	if err != nil {
		t.Fatal(err)
	}
	for download.Receive() {
	}
	if err := download.Err(); err != nil {
		t.Fatal(err)
	}
	if download.ResponseHeader().Get(requestid.Header) != "agent-download-206" {
		t.Fatal("download lost request ID")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// The original handlers emit two list logs and one log for each of
	// process start, upload, and download. Request correlation adds no logs.
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 5 {
		t.Fatalf("expected only the five existing operation logs, got %d: %s", len(lines), data)
	}
	for _, pair := range [][2]string{{"Received list processes request", "agent-unary-206"}, {"Received start process request", "agent-stream-206"}, {"Successfully uploaded file by stream", "agent-upload-206"}, {"Successfully streamed file", "agent-download-206"}} {
		found := false
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, pair[0]) {
				found = true
				if !strings.Contains(line, "request_id="+pair[1]) {
					t.Fatalf("uncorrelated log: %s", line)
				}
			}
		}
		if !found {
			t.Fatalf("missing operation log %q", pair[0])
		}
	}
	for _, path := range []string{"/health", "/missing"} {
		w := httptest.NewRecorder()
		newAgentHTTPHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if !requestid.Valid(w.Header().Get(requestid.Header)) {
			t.Fatalf("%s response lost generated ID", path)
		}
	}
}

func TestVsockRequestIDIsPerRequest(t *testing.T) {
	resetAgentAuth(t)
	h := newTestVsockHandler(nil, func() bool { return true })
	req := testInitRequest()
	for _, id := range []string{"init-first-206", "init-second-206", "invalid\nID"} {
		req.RequestID = id
		resp := h.HandleRequest(req)
		if resp.Status != "ready" || !requestid.Valid(resp.RequestID) {
			t.Fatalf("response = %#v", resp)
		}
		if requestid.Valid(id) && resp.RequestID != id {
			t.Fatal("vsock request ID changed")
		}
		if !requestid.Valid(id) && resp.RequestID == id {
			t.Fatal("invalid ID was echoed")
		}
	}
	if reqID := requestid.FromContext(context.Background()); reqID != "" {
		t.Fatal("request ID escaped request scope")
	}
	req.RequestID = ""
	if resp := h.HandleRequest(req); resp.RequestID != "" {
		t.Fatal("legacy response included unknown metadata")
	}

	failing := newTestVsockHandler(nil, func() bool { return true })
	req.Version++
	for _, id := range []string{"failed-first-206", "failed-second-206"} {
		req.RequestID = id
		resp := failing.HandleRequest(req)
		if resp.Status != "not_ready" || resp.RequestID != id {
			t.Fatalf("cached failure response = %#v", resp)
		}
	}
}

func TestProcessContextRetainsSourceIDAfterDisconnect(t *testing.T) {
	parent, cancelParent := context.WithCancel(requestid.With(context.Background(), "background-request-206"))
	ctx, cancel := processContext(parent, 50*time.Millisecond)
	defer cancel()
	cancelParent()
	if ctx.Err() != nil || requestid.FromContext(ctx) != "background-request-206" {
		t.Fatal("background process lost source correlation or was canceled")
	}
	select {
	case <-ctx.Done():
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("process context error: %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("process timeout was lost")
	}
}

func TestGuestReadinessProbePropagatesRequestID(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(requestid.Header) != "readiness-request-206" {
			t.Errorf("readiness probe ID = %q", r.Header.Get(requestid.Header))
		}
		_, _ = w.Write([]byte(HealthMsgOK))
	}))
	defer h.Close()
	if !checkAgentAPIHealthEndpointContext(requestid.With(context.Background(), "readiness-request-206"), h.URL) {
		t.Fatal("health probe failed")
	}
}
