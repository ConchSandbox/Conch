package protocol

import "github.com/openeuler/Conch/internal/netstack"

const (
	ProtocolVersion = 1
	MaxPayloadSize  = 16 << 10
)

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
	ErrorCode string `json:"errorCode,omitempty"`
	Message   string `json:"message,omitempty"`
}

func ReadyResponse() InitResponse {
	return InitResponse{Version: ProtocolVersion, Status: "ready"}
}

func NotReadyResponse(code, message string, retryable bool) InitResponse {
	return InitResponse{
		Version:   ProtocolVersion,
		Status:    "not_ready",
		Retryable: retryable,
		ErrorCode: code,
		Message:   message,
	}
}
