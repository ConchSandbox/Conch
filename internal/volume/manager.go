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

func (m *Manager) ValidateMounts(mounts []Mount, resumeBoot bool) error {
	if len(mounts) == 0 {
		return nil
	}
	if m == nil {
		return fmt.Errorf("volume manager is not configured")
	}
	if resumeBoot {
		return ErrInvalidMount.WrapMessage(
			fmt.Errorf("volume_mounts requested on a snapshot-resume (warm) template"),
			"volume_mounts are not supported on snapshot-resume (warm) templates; use a cold boot template instead",
		)
	}
	if len(mounts) > m.maxMounts {
		message := fmt.Sprintf("volume_mounts length %d exceeds configured maximum %d (volume.max_mounts)", len(mounts), m.maxMounts)
		return ErrInvalidMount.WrapMessage(fmt.Errorf("%s", message), message)
	}
	seenTargets := map[string]struct{}{}
	for _, mount := range mounts {
		target := filepath.Clean(strings.TrimSpace(mount.Path))
		if !filepath.IsAbs(target) {
			return ErrInvalidMount.Wrap(fmt.Errorf("volume mount path must be absolute: %s", mount.Path))
		}
		if isBlockedTarget(target) {
			return ErrInvalidMount.Wrap(fmt.Errorf("volume mount path is not allowed: %s", target))
		}
		if _, ok := seenTargets[target]; ok {
			return ErrInvalidMount.Wrap(fmt.Errorf("duplicate volume mount path: %s", target))
		}
		seenTargets[target] = struct{}{}

		source := filepath.Clean(strings.TrimSpace(mount.Source))
		if source == "" {
			return ErrInvalidMount.Wrap(fmt.Errorf("volume mount source must not be empty"))
		}
		if !filepath.IsAbs(source) {
			return ErrInvalidMount.Wrap(fmt.Errorf("volume mount source must be absolute: %s", source))
		}
	}
	return nil
}

func (m *Manager) PrepareSandbox(sandboxID string, mounts []Mount) ([]Device, error) {
	if err := m.ValidateMounts(mounts, false); err != nil {
		return nil, err
	}
	if len(mounts) == 0 {
		return nil, nil
	}
	return m.backend.Prepare(PrepareRequest{
		SandboxID: sandboxID,
		Mounts:    mounts,
	})
}

func (m *Manager) CleanupSandbox(sandboxID string, devices []Device) error {
	return m.backend.Cleanup(sandboxID, devices)
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
