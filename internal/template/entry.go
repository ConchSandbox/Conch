package template

import (
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
)

type Origin string

const (
	OriginImage      Origin = "image"
	OriginCheckpoint Origin = "checkpoint"
)

type BootMode string

const (
	BootModeCold   BootMode = "cold"
	BootModeResume BootMode = "resume"
)

// Entry is the non-persistent domain representation of a fully published and
// validated Template. An Entry has no lifecycle state: if it exists, it is
// ready to be consumed.
type Entry struct {
	Origin                Origin
	BootMode              BootMode
	BootIndexDigest       string
	ParentBootIndexDigest string
	SourceSandboxID       string
	SourceRef             string
	Labels                map[string]string
	CreatedAt             int64
}

// NormalizeEntry validates a complete Template and returns a defensive,
// canonical copy suitable for persistence.
func NormalizeEntry(entry Entry) (Entry, error) {
	switch entry.Origin {
	case OriginImage, OriginCheckpoint:
	default:
		return Entry{}, ErrInvalidArtifact.Wrap(fmt.Errorf("unknown template origin %q", entry.Origin))
	}
	switch entry.BootMode {
	case BootModeCold, BootModeResume:
	default:
		return Entry{}, ErrInvalidArtifact.Wrap(fmt.Errorf("unknown template boot mode %q", entry.BootMode))
	}
	rawDigest := strings.TrimSpace(entry.BootIndexDigest)
	parsed, err := digest.Parse(rawDigest)
	if err != nil {
		return Entry{}, ErrInvalidArtifact.Wrap(fmt.Errorf("invalid boot index digest %q: %w", rawDigest, err))
	}
	entry.BootIndexDigest = parsed.String()
	entry.ParentBootIndexDigest = strings.TrimSpace(entry.ParentBootIndexDigest)
	entry.SourceSandboxID = strings.TrimSpace(entry.SourceSandboxID)
	entry.SourceRef = strings.TrimSpace(entry.SourceRef)
	entry.Labels = copyMap(entry.Labels)
	return entry, nil
}
