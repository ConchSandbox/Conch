package guestd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	rootfsServicesReadyPath     = "/run/conch/services-ready"
	sandboxReadyResponseTimeout = 1500 * time.Millisecond
	agentAPIHealthTimeout       = 200 * time.Millisecond
)

var (
	agentAPIReady            atomic.Bool
	agentAPIReadyOnce        sync.Once
	agentAPIReadyCh          = make(chan struct{})
	rootfsServicesReady      atomic.Bool
	rootfsServicesReadyOnce  sync.Once
	rootfsServicesReadyCh    = make(chan struct{})
	rootfsMergeReady         atomic.Bool
	rootfsEntrypointExpected atomic.Bool
	agentAPIHealthURL        = "http://127.0.0.1" + ServerPort + "/health"
)

type VsockHandler interface {
	HandleRequest(request agentprotocol.InitRequest) agentprotocol.InitResponse
	NetworkReady() <-chan struct{}
	GetSandboxID() string
	SetSandboxID(id string)
}

type initState uint8

const (
	initAwaitingNetwork initState = iota // Waiting for the initial network configuration.
	initNetworkApplied                   // Network is configured; guest services are starting.
	initReady                            // Guest initialization completed; retries revalidate the network.
)

type VsockHandlerImpl struct {
	mu           sync.Mutex
	sandboxID    string
	healthFunc   func() bool
	applyNetwork func(netstack.GuestNetworkConfig, bool) error
	state        initState
	networkReady chan struct{}
	terminal     *agentprotocol.InitResponse
}

func NewVsockHandler(healthFunc func() bool, applyNetwork func(netstack.GuestNetworkConfig, bool) error) *VsockHandlerImpl {
	if applyNetwork == nil {
		applyNetwork = applyGuestNetworkConfig
	}
	return &VsockHandlerImpl{
		healthFunc:   healthFunc,
		applyNetwork: applyNetwork,
		networkReady: make(chan struct{}),
	}
}

func (h *VsockHandlerImpl) NetworkReady() <-chan struct{} {
	return h.networkReady
}

func (h *VsockHandlerImpl) HandleRequest(request agentprotocol.InitRequest) agentprotocol.InitResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.terminal != nil {
		return *h.terminal
	}
	if request.Version != agentprotocol.ProtocolVersion {
		return h.failTerminal("UNSUPPORTED_VERSION", fmt.Errorf("unsupported protocol version %d", request.Version))
	}
	if err := validateInitRequest(request); err != nil {
		return h.failTerminal("INVALID_REQUEST", err)
	}

	switch h.state {
	case initAwaitingNetwork:
		if err := h.applyNetwork(request.Network, false); err != nil {
			return h.failTerminal("NETWORK_CONFIG_FAILED", err)
		}
		h.applyIdentity(request)
		h.state = initNetworkApplied
		close(h.networkReady)
	case initReady:
		if err := h.applyNetwork(request.Network, true); err != nil {
			return h.failTerminal("NETWORK_MISMATCH", err)
		}
		h.applyIdentity(request)
		return agentprotocol.ReadyResponse()
	case initNetworkApplied:
		// A retry waits for the already-started services; it never starts them again.
	}

	if h.waitForReady(sandboxReadyResponseTimeout) {
		h.state = initReady
		return agentprotocol.ReadyResponse()
	}
	return agentprotocol.NotReadyResponse("SERVICES_STARTING", "sandbox services are not ready", true)
}

func validateInitRequest(request agentprotocol.InitRequest) error {
	if strings.TrimSpace(request.SandboxID) == "" {
		return fmt.Errorf("sandboxID is required")
	}
	if strings.TrimSpace(request.AgentToken) == "" {
		return fmt.Errorf("agentToken is required")
	}
	if len(request.AgentToken) > 4096 {
		return fmt.Errorf("agentToken is too large")
	}
	for key := range request.Env {
		if !validEnvKey(key) {
			return fmt.Errorf("invalid environment key %q", key)
		}
	}
	return request.Network.Validate()
}

func (h *VsockHandlerImpl) failTerminal(code string, err error) agentprotocol.InitResponse {
	response := agentprotocol.NotReadyResponse(code, err.Error(), false)
	h.terminal = &response
	return response
}

func (h *VsockHandlerImpl) applyIdentity(request agentprotocol.InitRequest) {
	agentAuth.SetToken(request.AgentToken)
	applyVsockEnv(request.Env)
	if h.sandboxID == request.SandboxID {
		return
	}
	h.sandboxID = request.SandboxID
	mu.Lock()
	currentSandboxID = request.SandboxID
	mu.Unlock()
	baseLogger := rootLogger
	if baseLogger == nil {
		baseLogger = ulog.GetLogger()
	}
	rootLogger = baseLogger.ReplaceField("sandboxId", request.SandboxID)
	ulog.SetLogger(rootLogger)
	ulog.GetLogger().Info("Updated sandbox_id from vsock", ulog.F("new_sandbox_id", request.SandboxID))
}

func validEnvKey(key string) bool {
	if key == "" || strings.Contains(key, "=") {
		return false
	}
	for _, r := range key {
		if r == 0 {
			return false
		}
	}
	return true
}

func applyVsockEnv(env map[string]string) {
	for key, value := range env {
		if err := os.Setenv(key, value); err != nil {
			ulog.GetLogger().Warn("failed to set sandbox environment variable",
				ulog.F("key", key),
				ulog.F("error", err),
			)
		}
	}
}

func (h *VsockHandlerImpl) waitForReady(timeout time.Duration) bool {
	if h.healthFunc() {
		return true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		if h.healthFunc() {
			return true
		}

		waitAgentAPI := !agentAPIReady.Load()
		waitRootfs := rootfsServicesRequired() && !rootfsServicesAreReady()
		if !waitAgentAPI && !waitRootfs {
			return h.healthFunc()
		}

		switch {
		case waitAgentAPI && waitRootfs:
			select {
			case <-agentAPIReadyCh:
			case <-rootfsServicesReadyCh:
			case <-timer.C:
				return h.healthFunc()
			}
		case waitAgentAPI:
			select {
			case <-agentAPIReadyCh:
			case <-timer.C:
				return h.healthFunc()
			}
		case waitRootfs:
			select {
			case <-rootfsServicesReadyCh:
			case <-timer.C:
				return h.healthFunc()
			}
		}
	}
}

func markAgentAPIReady() {
	agentAPIReady.Store(true)
	agentAPIReadyOnce.Do(func() {
		close(agentAPIReadyCh)
	})
}

func markAgentAPINotReady() {
	agentAPIReady.Store(false)
}

func (h *VsockHandlerImpl) GetSandboxID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sandboxID
}

func (h *VsockHandlerImpl) SetSandboxID(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sandboxID = id
}

func markRootfsServicesReady() {
	rootfsServicesReady.Store(true)
	rootfsServicesReadyOnce.Do(func() {
		close(rootfsServicesReadyCh)
	})
}

func markRootfsMergeReady() {
	rootfsMergeReady.Store(true)
}

func checkAgentAPIHealthEndpoint(url string) bool {
	client := http.Client{Timeout: agentAPIHealthTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return false
	}
	return strings.Contains(string(body), HealthMsgOK)
}

func checkSandboxReady() bool {
	if !checkAgentAPIHealthEndpoint(agentAPIHealthURL) {
		return false
	}
	if !rootfsMergeReady.Load() {
		return false
	}

	return !rootfsServicesRequired() || rootfsServicesAreReady()
}

func featureEnabled(name string) bool {
	for _, base := range []string{"", MergeTarget} {
		path := base + "/etc/conch/features/" + name
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func rootfsServicesRequired() bool {
	return rootfsEntrypointExpected.Load() && featureEnabled("envd")
}

func rootfsServicesAreReady() bool {
	if rootfsServicesReady.Load() {
		return true
	}

	if _, err := os.Stat(rootfsServicesReadyPath); err == nil {
		markRootfsServicesReady()
		return true
	}
	return false
}
