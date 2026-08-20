// Package memorymode validates the global sandbox memory policy before runtime
// resources are allocated.
package memorymode

import (
	"errors"
	"fmt"
	"strings"

	conchimage "github.com/openeuler/Conch/internal/image"
)

type Mode string

const (
	ModeFull        Mode = "full"
	ModeIncremental Mode = "incremental"
)

var ErrPrecondition = errors.New("memory precondition failed")

type Input struct {
	Mode           Mode
	VMMName        string
	Resume         bool
	ArtifactFormat string
}

func Validate(input Input) error {
	if input.Mode != ModeFull && input.Mode != ModeIncremental {
		return fmt.Errorf("unknown memory mode %q", input.Mode)
	}
	switch strings.TrimSpace(input.VMMName) {
	case "cloud-hypervisor":
		if input.Mode == ModeIncremental {
			return precondition("incremental mode is not supported by Cloud Hypervisor")
		}
		return nil
	case "stratovirt":
	default:
		return fmt.Errorf("unsupported VMM %q for memory mode %q", input.VMMName, input.Mode)
	}
	if input.Resume {
		switch input.ArtifactFormat {
		case conchimage.MemoryFormatFull:
			if input.Mode == ModeIncremental {
				return precondition("incremental mode cannot resume full artifact")
			}
			return nil
		case conchimage.MemoryFormatIncrementalV1:
			if input.Mode == ModeFull {
				return precondition("full mode cannot resume incremental-v1 artifact")
			}
			return nil
		default:
			return precondition("unsupported resume memory artifact format %q", input.ArtifactFormat)
		}
	}
	return nil
}

func precondition(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPrecondition, fmt.Sprintf(format, args...))
}
