package guestd

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	rootfsServicesReadyPath     = "/run/conch/services-ready"
	sandboxReadyResponseTimeout = 1500 * time.Millisecond
	agentAPIHealthTimeout       = 200 * time.Millisecond
	vsockEnvLinePrefix          = "ENV:"
	vsockEnvJSONLinePrefix      = "ENV_JSON:"
)

var (
	agentAPIReady            atomic.Bool
	agentAPIReadyOnce        sync.Once
	agentAPIReadyCh          = make(chan struct{})
	rootfsServicesReady      atomic.Bool
	rootfsServicesReadyOnce  sync.Once
	rootfsServicesReadyCh    = make(chan struct{})
	rootfsEntrypointExpected atomic.Bool
	agentAPIHealthURL        = "http://127.0.0.1" + ServerPort + "/health"
)

type VsockHandler interface {
	HandleMessage(message string) string
	GetSandboxID() string
	SetSandboxID(id string)
}

type VsockHandlerImpl struct {
	mu         sync.Mutex
	sandboxID  string
	version    string
	healthFunc func() bool
}

func NewVsockHandler(version string, healthFunc func() bool) *VsockHandlerImpl {
	return &VsockHandlerImpl{
		version:    version,
		healthFunc: healthFunc,
	}
}

func (h *VsockHandlerImpl) HandleMessage(message string) string {
	logger := ulog.GetLogger()

	if strings.Contains(message, "SANDBOX_ID:") {
		newSandboxID := parseVsockField(message, "SANDBOX_ID:")
		if newSandboxID != "" {
			agentToken := parseVsockField(message, "AGENT_TOKEN:")
			if agentToken == "" {
				logger.Warn("agent token missing from vsock init message")
				return "NOT_READY\n"
			}
			env, err := parseVsockEnv(message)
			if err != nil {
				logger.Warn("invalid environment in vsock init message", ulog.F("error", err))
				return "NOT_READY\n"
			}
			agentAuth.SetToken(agentToken)
			applyVsockEnv(env)

			if newSandboxID != "" {
				if h.GetSandboxID() != newSandboxID {
					h.SetSandboxID(newSandboxID)

					baseLogger := rootLogger
					if baseLogger == nil {
						baseLogger = logger
					}

					newCtxLogger := baseLogger.ReplaceField("sandboxId", newSandboxID)
					rootLogger = newCtxLogger
					ulog.SetLogger(newCtxLogger)

					logger = ulog.GetLogger()
					logger.Info("Updated sandbox_id from vsock",
						ulog.F("new_sandbox_id", newSandboxID),
					)
				}

				if h.waitForReady(sandboxReadyResponseTimeout) {
					response := "OK\nREADY:" + h.version + "\n"
					logger.Info("sandbox services healthy, sent READY back with version",
						ulog.F("version", h.version))
					return response
				} else {
					logger.Warn("sandbox services not ready before vsock response timeout",
						ulog.F("timeout", sandboxReadyResponseTimeout.String()))
					return "NOT_READY\n"
				}
			}
		}
	}
	return ""
}

func parseVsockEnv(message string) (map[string]string, error) {
	env := make(map[string]string)
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, vsockEnvLinePrefix):
			key, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, vsockEnvLinePrefix)), "=")
			if !ok {
				return nil, invalidVsockEnvEntry(line)
			}
			key = strings.TrimSpace(key)
			if !validEnvKey(key) {
				return nil, invalidVsockEnvEntry(line)
			}
			env[key] = value
		case strings.HasPrefix(line, vsockEnvJSONLinePrefix):
			var parsed map[string]string
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, vsockEnvJSONLinePrefix))), &parsed); err != nil {
				return nil, err
			}
			for key, value := range parsed {
				if !validEnvKey(key) {
					return nil, invalidVsockEnvEntry(key)
				}
				env[key] = value
			}
		}
	}
	return env, nil
}

func invalidVsockEnvEntry(value string) error {
	return &invalidVsockEnvError{value: value}
}

type invalidVsockEnvError struct {
	value string
}

func (e *invalidVsockEnvError) Error() string {
	return "invalid environment entry: " + e.value
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

func parseVsockField(message, prefix string) string {
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, prefix) {
			parts := strings.SplitN(line, prefix, 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
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
