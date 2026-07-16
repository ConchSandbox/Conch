package guestd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestVsockHandlerRequiresAgentToken(t *testing.T) {
	agentAuth.SetToken("")
	handler := NewVsockHandler("test-version", func() bool { return true })

	resp := handler.HandleMessage("I AM SANDBOX_ID:sandbox-1\n")
	if resp != "NOT_READY\n" {
		t.Fatalf("HandleMessage() = %q, want NOT_READY", resp)
	}
}

func TestVsockHandlerSetsAgentToken(t *testing.T) {
	agentAuth.SetToken("")
	t.Cleanup(func() {
		agentAuth.SetToken("")
	})
	handler := NewVsockHandler("test-version", func() bool { return true })

	resp := handler.HandleMessage("I AM SANDBOX_ID:sandbox-1\nAGENT_TOKEN:secret\n")
	if resp != "OK\nREADY:test-version\n" {
		t.Fatalf("HandleMessage() = %q, want READY response", resp)
	}
	header := http.Header{}
	header.Set(agentTokenHeaderKey, "secret")
	if err := agentAuth.verifyHTTPHeader(header); err != nil {
		t.Fatalf("agent token was not set from vsock message: %v", err)
	}
}

func TestVsockHandlerAppliesEnvironment(t *testing.T) {
	agentAuth.SetToken("")
	t.Cleanup(func() {
		agentAuth.SetToken("")
		_ = os.Unsetenv("CONCH_TEST_ENV")
		_ = os.Unsetenv("CONCH_TEST_JSON_ENV")
	})
	handler := NewVsockHandler("test-version", func() bool { return true })

	resp := handler.HandleMessage("I AM SANDBOX_ID:sandbox-1\nAGENT_TOKEN:secret\nENV:CONCH_TEST_ENV=line-value\nENV_JSON:{\"CONCH_TEST_JSON_ENV\":\"json-value\"}\n")
	if resp != "OK\nREADY:test-version\n" {
		t.Fatalf("HandleMessage() = %q, want READY response", resp)
	}
	if got := os.Getenv("CONCH_TEST_ENV"); got != "line-value" {
		t.Fatalf("CONCH_TEST_ENV = %q, want line-value", got)
	}
	if got := os.Getenv("CONCH_TEST_JSON_ENV"); got != "json-value" {
		t.Fatalf("CONCH_TEST_JSON_ENV = %q, want json-value", got)
	}
}

func TestVsockHandlerRejectsInvalidEnvironment(t *testing.T) {
	agentAuth.SetToken("")
	t.Cleanup(func() {
		agentAuth.SetToken("")
		_ = os.Unsetenv("CONCH_TEST_INVALID_ENV")
	})
	handler := NewVsockHandler("test-version", func() bool { return true })

	resp := handler.HandleMessage("I AM SANDBOX_ID:sandbox-1\nAGENT_TOKEN:secret\nENV:CONCH_TEST_INVALID_ENV\n")
	if resp != "NOT_READY\n" {
		t.Fatalf("HandleMessage() = %q, want NOT_READY", resp)
	}
	if got := os.Getenv("CONCH_TEST_INVALID_ENV"); got != "" {
		t.Fatalf("CONCH_TEST_INVALID_ENV = %q, want empty", got)
	}
	header := http.Header{}
	header.Set(agentTokenHeaderKey, "secret")
	if err := agentAuth.verifyHTTPHeader(header); err == nil {
		t.Fatal("agent token was set despite invalid environment")
	}
}

func TestCheckAgentAPIHealthEndpoint(t *testing.T) {
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	}))
	t.Cleanup(okServer.Close)

	if !checkAgentAPIHealthEndpoint(okServer.URL) {
		t.Fatal("checkAgentAPIHealthEndpoint() = false, want true")
	}

	badStatusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(badStatusServer.Close)

	if checkAgentAPIHealthEndpoint(badStatusServer.URL) {
		t.Fatal("checkAgentAPIHealthEndpoint() = true for bad status")
	}

	badBodyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"DOWN"}`))
	}))
	t.Cleanup(badBodyServer.Close)

	if checkAgentAPIHealthEndpoint(badBodyServer.URL) {
		t.Fatal("checkAgentAPIHealthEndpoint() = true for bad body")
	}
}

func TestCheckSandboxReadyUsesLoopbackEndpoint(t *testing.T) {
	oldURL := agentAPIHealthURL
	oldRootfsEntrypointExpected := rootfsEntrypointExpected.Load()
	t.Cleanup(func() {
		agentAPIHealthURL = oldURL
		rootfsEntrypointExpected.Store(oldRootfsEntrypointExpected)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	}))
	t.Cleanup(server.Close)
	agentAPIHealthURL = server.URL
	rootfsEntrypointExpected.Store(false)

	if !checkSandboxReady() {
		t.Fatal("checkSandboxReady() = false for healthy loopback endpoint")
	}

	badStatusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(badStatusServer.Close)
	agentAPIHealthURL = badStatusServer.URL

	if checkSandboxReady() {
		t.Fatal("checkSandboxReady() = true for unhealthy loopback endpoint")
	}
}
