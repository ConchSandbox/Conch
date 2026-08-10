// Package memorymode resolves the global sandbox memory policy before runtime
// resources are allocated.
package memorymode

import (
	"errors"
	"fmt"
	"strings"

	conchimage "github.com/openeuler/Conch/internal/image"
)

type RequestedMode string

const (
	RequestedFull        RequestedMode = "full"
	RequestedIncremental RequestedMode = "incremental"
)

type EffectiveMode string

const (
	EffectiveFull        EffectiveMode = "full"
	EffectiveIncremental EffectiveMode = "incremental"
)

var ErrPrecondition = errors.New("memory precondition failed")

type Input struct {
	Requested      RequestedMode
	VMMName        string
	Resume         bool
	ArtifactFormat string
}

func Resolve(input Input) (EffectiveMode, error) {
	if strings.TrimSpace(input.VMMName) == "cloud-hypervisor" {
		return EffectiveFull, nil
	}
	if input.Requested != RequestedFull && input.Requested != RequestedIncremental {
		return "", fmt.Errorf("unknown requested memory mode %q", input.Requested)
	}
	if input.Resume {
		switch input.ArtifactFormat {
		case conchimage.MemoryFormatFull:
			if input.Requested == RequestedIncremental {
				return "", precondition("incremental mode cannot resume full artifact")
			}
			return EffectiveFull, nil
		case conchimage.MemoryFormatIncrementalV1:
			if input.Requested == RequestedFull {
				return "", precondition("full mode cannot resume incremental-v1 artifact")
			}
			return EffectiveIncremental, nil
		default:
			return "", precondition("unsupported resume memory artifact format %q", input.ArtifactFormat)
		}
	}
	switch input.Requested {
	case RequestedFull:
		return EffectiveFull, nil
	case RequestedIncremental:
		return EffectiveIncremental, nil
	default:
		panic("validated requested mode")
	}
}

func precondition(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPrecondition, fmt.Sprintf(format, args...))
}
