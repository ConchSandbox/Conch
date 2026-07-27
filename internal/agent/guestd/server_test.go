package guestd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/openeuler/Conch/api/go_proto"
	agentconnect "github.com/openeuler/Conch/api/go_proto/pbconnect"
)

func TestAgentHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	handleAgentHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"status":"OK"`) {
		t.Fatalf("health body = %q, want OK status", rec.Body.String())
	}
}

func TestConnectStreamingJSONStartProcess(t *testing.T) {
	agentAuth.SetToken("secret")
	defer agentAuth.SetToken("")

	httpServer := httptest.NewServer(newAgentHTTPHandler())
	defer httpServer.Close()

	client := agentconnect.NewProcessServiceClient(httpServer.Client(), httpServer.URL)
	req := connect.NewRequest(&pb.StartProcessRequest{
		Cmd:  "sh",
		Args: []string{"-c", "echo connect-ok"},
		Cwd:  t.TempDir(),
	})
	req.Header().Set(agentTokenHeaderKey, "secret")
	stream, err := client.StartProcess(context.Background(), req)
	if err != nil {
		t.Fatalf("StartProcess() error = %v", err)
	}

	var stdout strings.Builder
	exited := false
	for stream.Receive() {
		event := stream.Msg()
		if data := event.GetData(); data != nil {
			stdout.Write(data.GetStdout())
		}
		if event.GetEnd() != nil {
			exited = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StartProcess() stream error = %v", err)
	}
	if !strings.Contains(stdout.String(), "connect-ok") {
		t.Fatalf("StartProcess() stdout = %q, want command stdout", stdout.String())
	}
	if !exited {
		t.Fatal("StartProcess() stream did not receive end event")
	}
}

func TestConnectStreamingStartProcessHonorsTimeoutHeader(t *testing.T) {
	agentAuth.SetToken("secret")
	defer agentAuth.SetToken("")

	httpServer := httptest.NewServer(newAgentHTTPHandler())
	defer httpServer.Close()

	client := agentconnect.NewProcessServiceClient(httpServer.Client(), httpServer.URL)
	req := connect.NewRequest(&pb.StartProcessRequest{
		Cmd:  "sleep",
		Args: []string{"5"},
		Cwd:  t.TempDir(),
	})
	req.Header().Set(agentTokenHeaderKey, "secret")
	req.Header().Set("Connect-Timeout-Ms", "50")

	started := time.Now()
	stream, err := client.StartProcess(context.Background(), req)
	if err != nil {
		t.Fatalf("StartProcess() error = %v, want stream", err)
	}
	for stream.Receive() {
	}
	if err := stream.Err(); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("StartProcess() stream error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("StartProcess() ran for %s, want less than one second", elapsed)
	}
}

func TestConnectUnaryRejectsNonStandardConnectJSONContentType(t *testing.T) {
	agentAuth.SetToken("secret")
	defer agentAuth.SetToken("")

	req := httptest.NewRequest(http.MethodPost, processListPath, strings.NewReader(`{}`))
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set(agentTokenHeaderKey, "secret")
	rec := httptest.NewRecorder()

	newAgentHTTPHandler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("application/connect+json status = %d, want non-OK", rec.Code)
	}
}
