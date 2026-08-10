package cow

import "fmt"

const (
	ProtocolVersion   = 1
	DefaultSocketPath = "/run/conch/cow.sock"
)

const (
	RequestCapabilities        = "Capabilities"
	RequestAttach              = "Attach"
	RequestWaitAttachmentReady = "WaitAttachmentReady"
	RequestDetach              = "Detach"
)

const (
	CapabilitySupported   = "supported"
	CapabilityUnsupported = "unsupported"
)

type Request struct {
	Type               string `json:"type"`
	ProtocolVersion    int    `json:"protocol_version"`
	RequestID          string `json:"request_id"`
	MemorySnapshotRoot string `json:"memory_snapshot_root,omitempty"`
	Token              string `json:"token,omitempty"`
	SandboxID          string `json:"sandbox_id,omitempty"`
}

type Capabilities struct {
	IncrementalMemory string   `json:"incremental_memory"`
	MissingFeatures   []string `json:"missing_features,omitempty"`
	ProbeError        string   `json:"probe_error,omitempty"`
}

type Response struct {
	OK              bool          `json:"ok"`
	ProtocolVersion int           `json:"protocol_version"`
	RequestID       string        `json:"request_id"`
	Error           string        `json:"error,omitempty"`
	Capabilities    *Capabilities `json:"capabilities,omitempty"`
	Token           string        `json:"token,omitempty"`
	UFFDSocketPath  string        `json:"uffd_socket_path,omitempty"`
	MemorySize      uint64        `json:"memory_size,omitempty"`
	BlockSize       uint64        `json:"block_size,omitempty"`
}

func validateRequest(request Request, fds []int) error {
	if request.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", request.ProtocolVersion)
	}
	if request.RequestID == "" {
		return fmt.Errorf("request ID is required")
	}
	expectedFDs := 0
	switch request.Type {
	case RequestCapabilities:
	case RequestAttach:
		if request.MemorySnapshotRoot == "" {
			return fmt.Errorf("memory snapshot root is required")
		}
	case RequestWaitAttachmentReady:
		if request.Token == "" || request.SandboxID == "" {
			return fmt.Errorf("token and sandbox ID are required")
		}
	case RequestDetach:
		if request.Token == "" {
			return fmt.Errorf("token is required")
		}
	default:
		return fmt.Errorf("unknown request type %q", request.Type)
	}
	if len(fds) != expectedFDs {
		return fmt.Errorf("%s request has %d descriptors, expected %d", request.Type, len(fds), expectedFDs)
	}
	return nil
}
