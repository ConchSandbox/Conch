// Package memorymode resolves the global sandbox memory policy before runtime
// resources are allocated.
package memorymode

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openeuler/Conch/internal/cow"
)

type RequestedMode string

const (
	RequestedAuto        RequestedMode = "auto"
	RequestedFull        RequestedMode = "full"
	RequestedIncremental RequestedMode = "incremental"
)

type EffectiveMode string

const (
	EffectiveFull        EffectiveMode = "full"
	EffectiveIncremental EffectiveMode = "incremental"
)

const (
	FormatFullV1        = "full-v1"
	FormatIncrementalV1 = "incremental-v1"
)

var ErrPrecondition = errors.New("memory precondition failed")

type Input struct {
	Requested      RequestedMode
	VMMName        string
	Resume         bool
	ArtifactFormat string
}

type CapabilityProvider interface {
	Capabilities(context.Context) (cow.Capabilities, error)
}

func Resolve(ctx context.Context, provider CapabilityProvider, input Input) (EffectiveMode, error) {
	if strings.TrimSpace(input.VMMName) == "cloud-hypervisor" {
		return EffectiveFull, nil
	}
	if input.Requested != RequestedAuto && input.Requested != RequestedFull && input.Requested != RequestedIncremental {
		return "", fmt.Errorf("unknown requested memory mode %q", input.Requested)
	}
	if input.Resume {
		switch input.ArtifactFormat {
		case FormatFullV1:
			if input.Requested == RequestedIncremental {
				return "", precondition("incremental mode cannot resume full-v1 artifact")
			}
			return EffectiveFull, nil
		case FormatIncrementalV1:
			if input.Requested == RequestedFull {
				return "", precondition("full mode cannot resume incremental-v1 artifact")
			}
			return incremental(ctx, provider)
		default:
			return "", precondition("unsupported resume memory artifact format %q", input.ArtifactFormat)
		}
	}
	switch input.Requested {
	case RequestedFull:
		return EffectiveFull, nil
	case RequestedIncremental:
		return incremental(ctx, provider)
	case RequestedAuto:
		return automatic(ctx, provider)
	default:
		panic("validated requested mode")
	}
}

func automatic(ctx context.Context, provider CapabilityProvider) (EffectiveMode, error) {
	capabilities, err := getCapabilities(ctx, provider)
	if err != nil {
		return "", err
	}
	switch capabilities.IncrementalMemory {
	case cow.CapabilitySupported:
		return EffectiveIncremental, nil
	case cow.CapabilityUnsupported:
		return EffectiveFull, nil
	default:
		return "", fmt.Errorf("cow reported unknown incremental memory capability %q", capabilities.IncrementalMemory)
	}
}

func incremental(ctx context.Context, provider CapabilityProvider) (EffectiveMode, error) {
	capabilities, err := getCapabilities(ctx, provider)
	if err != nil {
		return "", err
	}
	switch capabilities.IncrementalMemory {
	case cow.CapabilitySupported:
		return EffectiveIncremental, nil
	case cow.CapabilityUnsupported:
		return "", precondition("cow does not support incremental memory")
	default:
		return "", fmt.Errorf("cow reported unknown incremental memory capability %q", capabilities.IncrementalMemory)
	}
}

func getCapabilities(ctx context.Context, provider CapabilityProvider) (cow.Capabilities, error) {
	if provider == nil {
		return cow.Capabilities{}, fmt.Errorf("cow capability provider is not configured")
	}
	capabilities, err := provider.Capabilities(ctx)
	if err != nil {
		return cow.Capabilities{}, fmt.Errorf("get cow capabilities: %w", err)
	}
	return capabilities, nil
}

func precondition(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPrecondition, fmt.Sprintf(format, args...))
}
