package guestd

import (
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
)

var (
	grpcReady                atomic.Bool
	grpcReadyOnce            sync.Once
	grpcReadyCh              = make(chan struct{})
	rootfsServicesReady      atomic.Bool
	rootfsServicesReadyOnce  sync.Once
	rootfsServicesReadyCh    = make(chan struct{})
	rootfsEntrypointExpected atomic.Bool
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
		parts := strings.Split(message, "SANDBOX_ID:")
		if len(parts) > 1 {
			newSandboxID := strings.TrimSpace(parts[1])
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

		waitGRPC := !grpcReady.Load()
		waitRootfs := rootfsServicesRequired() && !rootfsServicesAreReady()
		if !waitGRPC && !waitRootfs {
			return h.healthFunc()
		}

		switch {
		case waitGRPC && waitRootfs:
			select {
			case <-grpcReadyCh:
			case <-rootfsServicesReadyCh:
			case <-timer.C:
				return h.healthFunc()
			}
		case waitGRPC:
			select {
			case <-grpcReadyCh:
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

func markGRPCReady() {
	grpcReady.Store(true)
	grpcReadyOnce.Do(func() {
		close(grpcReadyCh)
	})
}

func markGRPCNotReady() {
	grpcReady.Store(false)
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

func checkGRPCHealth() bool {
	mu.Lock()
	safe := isSafe
	mu.Unlock()
	return safe && grpcReady.Load()
}

func checkSandboxReady() bool {
	if !checkGRPCHealth() {
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
