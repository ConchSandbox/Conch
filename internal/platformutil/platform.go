package platformutil

import (
	"fmt"
	"strings"

	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Parse selects the daemon's platform when no explicit platform is supplied.
func Parse(value string) (ocispec.Platform, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = platforms.DefaultString()
	}
	p, err := platforms.Parse(value)
	if err != nil {
		return ocispec.Platform{}, fmt.Errorf("invalid platform %q: %w", value, err)
	}
	p = platforms.Normalize(p)
	if p.OS != "linux" || (p.Architecture != "amd64" && p.Architecture != "arm64") || p.Variant != "" {
		return ocispec.Platform{}, fmt.Errorf("unsupported platform %q: use linux/amd64 or linux/arm64", value)
	}
	return p, nil
}

func Normalize(value string) (string, error) {
	p, err := Parse(value)
	if err != nil {
		return "", err
	}
	return platforms.Format(p), nil
}
