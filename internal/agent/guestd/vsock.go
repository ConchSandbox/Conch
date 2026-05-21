package guestd

import (
	"strings"
	"sync"

	"github.com/openeuler/Conch/pkg/ulog"
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
	logger.Info("Handling vsock message", ulog.F("message", message))

	if strings.Contains(message, "SANDBOX_ID:") {
		parts := strings.Split(message, "SANDBOX_ID:")
		if len(parts) > 1 {
			newSandboxID := strings.TrimSpace(parts[1])
			if newSandboxID != "" {
				h.SetSandboxID(newSandboxID)

				newCtxLogger := rootLogger.ReplaceField("sandboxId", newSandboxID)
				ulog.SetLogger(newCtxLogger)

				logger = ulog.GetLogger()
				logger.Info("Updated sandbox_id from vsock", ulog.F("new_sandbox_id", newSandboxID))

				if h.healthFunc() {
					response := "OK\nREADY:" + h.version + "\n"
					logger.Info("gRPC and network healthy, sent READY back with version", ulog.F("version", h.version))
					return response
				} else {
					logger.Error("gRPC or network not responding")
					return "NOT_READY\n"
				}
			}
		}
	}
	return ""
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

func checkGRPCHealth() bool {
	mu.Lock()
	defer mu.Unlock()
	return isSafe
}
