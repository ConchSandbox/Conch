package template

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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

var ErrAlreadyExists = errors.New("template already exists")

// Entry is the non-persistent domain representation of a fully published and
// validated Template. An Entry has no lifecycle state: if it exists, it is
// ready to be consumed.
type Entry struct {
	ID               string
	Origin           Origin
	BootMode         BootMode
	BootIndexDigest  string
	ParentTemplateID string
	SourceSandboxID  string
	ImageName        string
	BuildRef         string
	Labels           map[string]string
	CreatedAt        int64
}

// NewID generates a Template identity without reading or mutating storage.
func NewID() (string, error) {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate template id: %w", err)
	}
	return "tmpl_" + hex.EncodeToString(data[:]), nil
}

// NormalizeEntry validates a complete Template and returns a defensive,
// canonical copy suitable for persistence.
func NormalizeEntry(entry Entry) (Entry, error) {
	entry.ID = strings.TrimSpace(entry.ID)
	if entry.ID == "" {
		return Entry{}, fmt.Errorf("template id is required")
	}
	switch entry.Origin {
	case OriginImage, OriginCheckpoint:
	default:
		return Entry{}, fmt.Errorf("unknown template origin %q", entry.Origin)
	}
	switch entry.BootMode {
	case BootModeCold, BootModeResume:
	default:
		return Entry{}, fmt.Errorf("unknown template boot mode %q", entry.BootMode)
	}
	rawDigest := strings.TrimSpace(entry.BootIndexDigest)
	parsed, err := digest.Parse(rawDigest)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid boot index digest %q: %w", rawDigest, err)
	}
	entry.BootIndexDigest = parsed.String()
	entry.ParentTemplateID = strings.TrimSpace(entry.ParentTemplateID)
	entry.SourceSandboxID = strings.TrimSpace(entry.SourceSandboxID)
	entry.ImageName = strings.TrimSpace(entry.ImageName)
	entry.BuildRef = strings.TrimSpace(entry.BuildRef)
	entry.Labels = copyMap(entry.Labels)
	return entry, nil
}
