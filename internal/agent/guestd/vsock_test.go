package guestd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/internal/netstack"
)

func TestVsockHandlerRequiresAgentToken(t *testing.T) {
	resetAgentAuth(t)
	handler := newTestVsockHandler(nil, func() bool { return true })
	request := testInitRequest()
	request.AgentToken = ""

	response := handler.HandleRequest(request)
	if response.Status != "not_ready" || response.Retryable || !strings.Contains(response.Message, "agentToken is required") {
		t.Fatalf("HandleRequest() = %#v, want terminal validation failure", response)
	}
}

func TestVsockHandlerRejectsUnsupportedVersion(t *testing.T) {
	resetAgentAuth(t)
	handler := newTestVsockHandler(nil, func() bool { return true })
	request := testInitRequest()
	request.Version++

	response := handler.HandleRequest(request)
	if response.Status != "not_ready" || response.Retryable || !strings.Contains(response.Message, "unsupported protocol version") {
		t.Fatalf("HandleRequest() = %#v, want terminal version failure", response)
	}
}

func TestVsockHandlerAppliesNetworkIdentityAndEnvironment(t *testing.T) {
	resetAgentAuth(t)
	t.Cleanup(func() {
		_ = os.Unsetenv("CONCH_TEST_ENV")
	})
	var gotConfig netstack.GuestNetworkConfig
	var gotRevalidate bool
	handler := newTestVsockHandler(func(cfg netstack.GuestNetworkConfig, revalidate bool) error {
		gotConfig = cfg
		gotRevalidate = revalidate
		return nil
	}, func() bool { return true })
	request := testInitRequest()
	request.Env = map[string]string{"CONCH_TEST_ENV": "value"}

	response := handler.HandleRequest(request)
	if response.Status != "ready" {
		t.Fatalf("HandleRequest() = %#v, want ready", response)
	}
	if gotRevalidate || !reflect.DeepEqual(gotConfig, request.Network) {
		t.Fatalf("applyNetwork() = (%#v, %v), want cold config", gotConfig, gotRevalidate)
	}
	if got := os.Getenv("CONCH_TEST_ENV"); got != "value" {
		t.Fatalf("CONCH_TEST_ENV = %q, want value", got)
	}
	if handler.GetSandboxID() != request.SandboxID {
		t.Fatalf("sandbox ID = %q, want %q", handler.GetSandboxID(), request.SandboxID)
	}
	header := http.Header{}
	header.Set(agentTokenHeaderKey, request.AgentToken)
	if err := agentAuth.verifyHTTPHeader(header); err != nil {
		t.Fatalf("agent token was not set: %v", err)
	}
	select {
	case <-handler.NetworkReady():
	default:
		t.Fatal("network ready channel was not closed")
	}
}

func TestVsockHandlerRetryRevalidatesNetwork(t *testing.T) {
	resetAgentAuth(t)
	var calls []bool
	handler := newTestVsockHandler(func(_ netstack.GuestNetworkConfig, revalidate bool) error {
		calls = append(calls, revalidate)
		return nil
	}, func() bool { return true })
	request := testInitRequest()

	if response := handler.HandleRequest(request); response.Status != "ready" {
		t.Fatalf("cold response = %#v, want ready", response)
	}
	if response := handler.HandleRequest(request); response.Status != "ready" {
		t.Fatalf("retry response = %#v, want ready", response)
	}
	if !reflect.DeepEqual(calls, []bool{false, true}) {
		t.Fatalf("revalidate calls = %v, want [false true]", calls)
	}
}

func TestVsockHandlerNetworkFailureIsTerminal(t *testing.T) {
	resetAgentAuth(t)
	handler := newTestVsockHandler(func(_ netstack.GuestNetworkConfig, _ bool) error {
		return errors.New("boom")
	}, func() bool { return true })

	first := handler.HandleRequest(testInitRequest())
	second := handler.HandleRequest(testInitRequest())
	if first.Status != "not_ready" || first.Retryable || first.Message != "boom" || !reflect.DeepEqual(first, second) {
		t.Fatalf("responses = %#v, %#v; want stable terminal failure", first, second)
	}
}

func TestVsockHandlerRevalidationMismatchIsTerminal(t *testing.T) {
	resetAgentAuth(t)
	handler := newTestVsockHandler(func(_ netstack.GuestNetworkConfig, revalidate bool) error {
		if revalidate {
			return errors.New("address mismatch")
		}
		return nil
	}, func() bool { return true })
	request := testInitRequest()
	if response := handler.HandleRequest(request); response.Status != "ready" {
		t.Fatalf("cold response = %#v, want ready", response)
	}
	response := handler.HandleRequest(request)
	if response.Status != "not_ready" || response.Retryable || response.Message != "address mismatch" {
		t.Fatalf("revalidation response = %#v, want terminal mismatch", response)
	}
}

func TestVsockHandlerRejectsInvalidEnvironment(t *testing.T) {
	for _, env := range []map[string]string{
		{"BAD=KEY": "value"},
		{"KEY": "bad\x00value"},
	} {
		resetAgentAuth(t)
		handler := newTestVsockHandler(nil, func() bool { return true })
		request := testInitRequest()
		request.Env = env

		response := handler.HandleRequest(request)
		if response.Status != "not_ready" || response.Retryable || !strings.Contains(response.Message, agentprotocol.ErrInvalidEnvironment.Error()) {
			t.Fatalf("HandleRequest(%q) = %#v, want terminal validation failure", env, response)
		}
		select {
		case <-handler.NetworkReady():
			t.Fatalf("HandleRequest(%q) applied network before rejecting environment", env)
		default:
		}
	}
}

func newTestVsockHandler(apply func(netstack.GuestNetworkConfig, bool) error, health func() bool) *VsockHandlerImpl {
	if apply == nil {
		apply = func(netstack.GuestNetworkConfig, bool) error { return nil }
	}
	return NewVsockHandler(health, apply)
}

func testInitRequest() agentprotocol.InitRequest {
	return agentprotocol.InitRequest{
		Version:    agentprotocol.ProtocolVersion,
		SandboxID:  "sandbox-1",
		AgentToken: "secret",
		Network: netstack.GuestNetworkConfig{
			GuestIP:      "192.168.100.21",
			PrefixLength: 24,
			Gateway:      "192.168.100.2",
			DNS:          netstack.DNSConfig{Nameservers: []string{"10.0.0.53"}},
		},
	}
}

func resetAgentAuth(t *testing.T) {
	t.Helper()
	agentAuth.SetToken("")
	t.Cleanup(func() { agentAuth.SetToken("") })
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
	oldRootfsMergeReady := rootfsMergeReady.Load()
	oldRootfsEntrypointExpected := rootfsEntrypointExpected.Load()
	t.Cleanup(func() {
		agentAPIHealthURL = oldURL
		rootfsMergeReady.Store(oldRootfsMergeReady)
		rootfsEntrypointExpected.Store(oldRootfsEntrypointExpected)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	}))
	t.Cleanup(server.Close)
	agentAPIHealthURL = server.URL
	rootfsMergeReady.Store(true)
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

func TestCheckSandboxReadyRequiresRootfsMerge(t *testing.T) {
	oldURL := agentAPIHealthURL
	oldRootfsMergeReady := rootfsMergeReady.Load()
	oldRootfsEntrypointExpected := rootfsEntrypointExpected.Load()
	t.Cleanup(func() {
		agentAPIHealthURL = oldURL
		rootfsMergeReady.Store(oldRootfsMergeReady)
		rootfsEntrypointExpected.Store(oldRootfsEntrypointExpected)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	}))
	t.Cleanup(server.Close)

	agentAPIHealthURL = server.URL
	rootfsEntrypointExpected.Store(false)
	rootfsMergeReady.Store(false)

	if checkSandboxReady() {
		t.Fatal("checkSandboxReady() = true before rootfs merge")
	}

	markRootfsMergeReady()
	if !checkSandboxReady() {
		t.Fatal("checkSandboxReady() = false after rootfs merge with healthy control plane")
	}
}

func TestCheckSandboxReadyDoesNotRequireRootfsServicesWhenAbsent(t *testing.T) {
	oldURL := agentAPIHealthURL
	oldRootfsMergeReady := rootfsMergeReady.Load()
	oldRootfsEntrypointExpected := rootfsEntrypointExpected.Load()
	oldRootfsServicesReady := rootfsServicesReady.Load()
	t.Cleanup(func() {
		agentAPIHealthURL = oldURL
		rootfsMergeReady.Store(oldRootfsMergeReady)
		rootfsEntrypointExpected.Store(oldRootfsEntrypointExpected)
		rootfsServicesReady.Store(oldRootfsServicesReady)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	}))
	t.Cleanup(server.Close)

	agentAPIHealthURL = server.URL
	rootfsMergeReady.Store(true)
	rootfsEntrypointExpected.Store(false)
	rootfsServicesReady.Store(false)

	if !checkSandboxReady() {
		t.Fatal("checkSandboxReady() = false without rootfs services")
	}
}
