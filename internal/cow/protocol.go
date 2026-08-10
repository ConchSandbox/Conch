package cow

import (
	"encoding/json"
	"fmt"
)

const (
	ProtocolVersion = 1
)

const (
	RequestPing                = "Ping"
	RequestAttach              = "Attach"
	RequestWaitAttachmentReady = "WaitAttachmentReady"
	RequestDetach              = "Detach"
)

type requestEnvelope struct {
	Type            string          `json:"type"`
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	Params          json.RawMessage `json:"params"`
}

type responseEnvelope struct {
	OK              bool            `json:"ok"`
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	Error           string          `json:"error,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
}

type PingRequest struct{}

type PingResponse struct{}

type AttachRequest struct {
	MemorySnapshotRoot string `json:"memory_snapshot_root"`
	SandboxID          string `json:"sandbox_id"`
}

type AttachResponse struct {
	Token          string `json:"token"`
	UFFDSocketPath string `json:"uffd_socket_path"`
	MemorySize     uint64 `json:"memory_size"`
	BlockSize      uint64 `json:"block_size"`
}

type WaitAttachmentReadyRequest struct {
	Token     string `json:"token"`
	SandboxID string `json:"sandbox_id"`
}

type WaitAttachmentReadyResponse struct{}

type DetachRequest struct {
	Token string `json:"token"`
}

type DetachResponse struct{}

func validateRequestEnvelope(request requestEnvelope, fds []int) error {
	if request.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", request.ProtocolVersion)
	}
	if request.RequestID == "" {
		return fmt.Errorf("request ID is required")
	}
	switch request.Type {
	case RequestPing, RequestAttach, RequestWaitAttachmentReady, RequestDetach:
	default:
		return fmt.Errorf("unknown request type %q", request.Type)
	}
	if len(fds) != 0 {
		return fmt.Errorf("%s request has %d descriptors, expected 0", request.Type, len(fds))
	}
	return nil
}
