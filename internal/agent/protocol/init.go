package protocol

import (
	"errors"
	"fmt"
	"strings"

	"github.com/openeuler/Conch/internal/netstack"
)

const (
	ProtocolVersion = 1
	MaxPayloadSize  = 16 << 10
)

// ErrInvalidEnvironment reports an environment key or value that cannot be
// represented in a process environment.
var ErrInvalidEnvironment = errors.New("invalid sandbox environment")

type InitRequest struct {
	Version    int                         `json:"version"`
	SandboxID  string                      `json:"sandboxID"`
	AgentToken string                      `json:"agentToken"`
	Env        map[string]string           `json:"env,omitempty"`
	Network    netstack.GuestNetworkConfig `json:"network"`
}

type InitResponse struct {
	Version   int    `json:"version"`
	Status    string `json:"status"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message,omitempty"`
}

// ValidateEnvironment rejects entries that os.Setenv cannot apply. Keeping
// this validation in the shared protocol package lets the control plane fail
// before starting a VM while the guest independently validates untrusted input.
func ValidateEnvironment(env map[string]string) error {
	for key, value := range env {
		if key == "" || strings.Contains(key, "=") || strings.ContainsRune(key, '\x00') {
			return fmt.Errorf("%w: invalid environment key %q", ErrInvalidEnvironment, key)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%w: value for %q contains NUL", ErrInvalidEnvironment, key)
		}
	}
	return nil
}

func ReadyResponse() InitResponse {
	return InitResponse{Version: ProtocolVersion, Status: "ready"}
}

func NotReadyResponse(message string, retryable bool) InitResponse {
	return InitResponse{
		Version:   ProtocolVersion,
		Status:    "not_ready",
		Retryable: retryable,
		Message:   message,
	}
}
