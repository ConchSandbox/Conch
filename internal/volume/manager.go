package volume

import (
	"fmt"
	"path/filepath"
	"strings"
)

type Manager struct {
	maxMounts int
	backend   Backend
}

// NewManager builds a Manager. MaxMounts is taken verbatim from cfg; the
// config layer (config.DefaultConfig + applyDefaults) is the single source of
// truth for the default, so no hardcoded fallback lives here. Returns an
// error if cfg.Backend names an unsupported backend.
func NewManager(cfg Config) (*Manager, error) {
	backend := strings.TrimSpace(cfg.Backend)
	if backend == "" {
		backend = DefaultBackend
	}
	if backend != DefaultBackend {
		return nil, fmt.Errorf("unsupported volume backend %q (only %q)", backend, DefaultBackend)
	}
	return &Manager{
		maxMounts: cfg.MaxMounts,
		backend:   NewVirtiofsBackend(cfg.Virtiofs),
	}, nil
}

func (m *Manager) PrepareSandbox(sandboxID string, mounts []Mount) (PreparedSandbox, error) {
	if len(mounts) == 0 {
		return PreparedSandbox{}, nil
	}
	if len(mounts) > m.maxMounts {
		return PreparedSandbox{}, fmt.Errorf("volumeMounts exceeds limit %d: %d", m.maxMounts, len(mounts))
	}
	if err := ValidateSegment(sandboxID, "sandbox_id"); err != nil {
		return PreparedSandbox{}, err
	}
	seenTargets := map[string]struct{}{}
	for _, mount := range mounts {
		target := filepath.Clean(strings.TrimSpace(mount.Path))
		if !filepath.IsAbs(target) {
			return PreparedSandbox{}, fmt.Errorf("volume mount path must be absolute: %s", mount.Path)
		}
		if isBlockedTarget(target) {
			return PreparedSandbox{}, fmt.Errorf("volume mount path is not allowed: %s", target)
		}
		if _, ok := seenTargets[target]; ok {
			return PreparedSandbox{}, fmt.Errorf("duplicate volume mount path: %s", target)
		}
		seenTargets[target] = struct{}{}

		source := filepath.Clean(strings.TrimSpace(mount.Source))
		if source == "" {
			return PreparedSandbox{}, fmt.Errorf("volume mount source must not be empty")
		}
		if !filepath.IsAbs(source) {
			return PreparedSandbox{}, fmt.Errorf("volume mount source must be absolute: %s", source)
		}
	}
	return m.backend.Prepare(PrepareRequest{
		SandboxID: sandboxID,
		Mounts:    mounts,
	})
}

func (m *Manager) CleanupSandbox(sandboxID string, prepared PreparedSandbox) error {
	return m.backend.Cleanup(sandboxID, prepared)
}

// CleanupStaleResources removes per-sandbox volume backends left by a killed
// daemon. The runtime directory is exclusively owned by Conch.
func (m *Manager) CleanupStaleResources() error {
	if m == nil || m.backend == nil {
		return nil
	}
	return m.backend.CleanupStaleResources()
}

func isBlockedTarget(target string) bool {
	switch target {
	case "/", "/proc", "/sys", "/dev", "/run":
		return true
	}
	return strings.HasPrefix(target, "/proc/") ||
		strings.HasPrefix(target, "/sys/") ||
		strings.HasPrefix(target, "/dev/") ||
		strings.HasPrefix(target, "/run/")
}
