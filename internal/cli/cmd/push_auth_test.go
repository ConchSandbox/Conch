package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/image/client"
)

func TestRunImagePushPromptsForCredentialsOnAuthError(t *testing.T) {
	oldPrompt := promptRegistryCredentials
	defer func() { promptRegistryCredentials = oldPrompt }()
	prompted := 0
	promptRegistryCredentials = func(_ context.Context, remoteImage, username string) (string, string, error) {
		prompted++
		if remoteImage != "remote/demo:latest" {
			t.Fatalf("prompt remote image = %q", remoteImage)
		}
		if username != "" {
			t.Fatalf("prompt username = %q, want empty", username)
		}
		return "demo-user", "demo-pass", nil
	}

	var calls int
	var retryReq client.PushImageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/image/push" {
			http.NotFound(w, r)
			return
		}
		if calls == 1 {
			http.Error(w, "authorization failed: no basic auth credentials", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&retryReq); err != nil {
			t.Fatalf("decode retry request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()
	t.Setenv("CONCH_API_URL", server.URL)

	if err := RunImagePush(context.Background(), []string{"localhost/demo:latest", "remote/demo:latest"}); err != nil {
		t.Fatalf("RunImagePush: %v", err)
	}
	if prompted != 1 || calls != 2 {
		t.Fatalf("prompted=%d calls=%d, want 1 and 2", prompted, calls)
	}
	if retryReq.Username != "demo-user" || retryReq.Password != "demo-pass" {
		t.Fatalf("retry request credentials = (%q, %q)", retryReq.Username, retryReq.Password)
	}
}

func TestRunImagePushPromptsForPasswordWhenUsernameProvided(t *testing.T) {
	oldPrompt := promptRegistryCredentials
	defer func() { promptRegistryCredentials = oldPrompt }()
	promptRegistryCredentials = func(_ context.Context, remoteImage, username string) (string, string, error) {
		if remoteImage != "remote/demo:latest" {
			t.Fatalf("prompt remote image = %q", remoteImage)
		}
		if username != "demo-user" {
			t.Fatalf("prompt username = %q, want demo-user", username)
		}
		return username, "demo-pass", nil
	}

	var calls int
	var retryReq client.PushImageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&retryReq); err != nil {
			t.Fatalf("decode retry request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()
	t.Setenv("CONCH_API_URL", server.URL)

	if err := RunImagePush(context.Background(), []string{"--username", "demo-user", "localhost/demo:latest", "remote/demo:latest"}); err != nil {
		t.Fatalf("RunImagePush: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if retryReq.Username != "demo-user" || retryReq.Password != "demo-pass" {
		t.Fatalf("retry request credentials = (%q, %q)", retryReq.Username, retryReq.Password)
	}
}

func TestRunTemplatePushPromptsForCredentialsOnAuthError(t *testing.T) {
	oldPrompt := promptRegistryCredentials
	defer func() { promptRegistryCredentials = oldPrompt }()
	prompted := 0
	promptRegistryCredentials = func(_ context.Context, remoteImage, username string) (string, string, error) {
		prompted++
		if remoteImage != "remote/template:latest" {
			t.Fatalf("prompt remote image = %q", remoteImage)
		}
		if username != "" {
			t.Fatalf("prompt username = %q, want empty", username)
		}
		return "template-user", "template-pass", nil
	}

	var calls int
	var retryReq client.TemplatePushRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/template/push" {
			http.NotFound(w, r)
			return
		}
		if calls == 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&retryReq); err != nil {
			t.Fatalf("decode retry request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()
	t.Setenv("CONCH_API_URL", server.URL)

	if err := runTemplatePush(context.Background(), []string{"tmpl_demo", "remote/template:latest"}); err != nil {
		t.Fatalf("runTemplatePush: %v", err)
	}
	if prompted != 1 || calls != 2 {
		t.Fatalf("prompted=%d calls=%d, want 1 and 2", prompted, calls)
	}
	if retryReq.Username != "template-user" || retryReq.Password != "template-pass" {
		t.Fatalf("retry request credentials = (%q, %q)", retryReq.Username, retryReq.Password)
	}
}

func TestPushWithRegistryAuthDoesNotPromptWithExplicitPassword(t *testing.T) {
	oldPrompt := promptRegistryCredentials
	defer func() { promptRegistryCredentials = oldPrompt }()
	promptRegistryCredentials = func(context.Context, string, string) (string, string, error) {
		t.Fatal("promptRegistryCredentials should not be called")
		return "", "", nil
	}

	wantErr := errors.New("status 401: unauthorized")
	calls := 0
	err := pushWithRegistryAuth(context.Background(), "remote/demo:latest", "demo-user", "bad-password", func(username, password string) error {
		calls++
		if username != "demo-user" || password != "bad-password" {
			t.Fatalf("credentials = (%q, %q)", username, password)
		}
		return wantErr
	})

	if !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("error=%v calls=%d, want original error and one call", err, calls)
	}
}

func TestPushWithRegistryAuthDoesNotPromptOnNonAuthError(t *testing.T) {
	oldPrompt := promptRegistryCredentials
	defer func() { promptRegistryCredentials = oldPrompt }()
	promptRegistryCredentials = func(context.Context, string, string) (string, string, error) {
		t.Fatal("promptRegistryCredentials should not be called")
		return "", "", nil
	}

	wantErr := errors.New("connection refused")
	calls := 0
	err := pushWithRegistryAuth(context.Background(), "remote/demo:latest", "", "", func(string, string) error {
		calls++
		return wantErr
	})

	if !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("error=%v calls=%d, want original error and one call", err, calls)
	}
}

func TestPushWithRegistryAuthCancelsCredentialPrompt(t *testing.T) {
	oldPrompt := promptRegistryCredentials
	defer func() { promptRegistryCredentials = oldPrompt }()
	promptRegistryCredentials = func(ctx context.Context, _, _ string) (string, string, error) {
		<-ctx.Done()
		return "", "", ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := pushWithRegistryAuth(ctx, "remote/demo:latest", "", "", func(string, string) error {
		calls++
		return errors.New("unauthorized")
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("push calls = %d, want no retry after cancellation", calls)
	}
}

func TestReadTerminalLineReturnsWhenContextIsCanceled(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	defer reader.Close()
	defer writer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := readTerminalLine(ctx, reader.Fd())
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readTerminalLine() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("readTerminalLine() did not return after context cancellation")
	}
}
