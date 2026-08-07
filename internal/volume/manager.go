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

func (m *Manager) PrepareSandbox(namespace, sandboxID string, mounts []Mount) ([]Device, error) {
	return m.prepareSandbox(namespace, sandboxID, mounts, nil)
}

// PrepareSandboxWithHealth prepares the sandbox volume and reports an
// unexpected backend exit after preparation completes. The callback must not
// call CleanupSandbox inline before the backend monitor has published exit
// completion; the built-in backend guarantees that publication happens first.
func (m *Manager) PrepareSandboxWithHealth(namespace, sandboxID string, mounts []Mount, onUnhealthy func(error)) ([]Device, error) {
	return m.prepareSandbox(namespace, sandboxID, mounts, onUnhealthy)
}

func (m *Manager) prepareSandbox(namespace, sandboxID string, mounts []Mount, onUnhealthy func(error)) ([]Device, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	if len(mounts) > m.maxMounts {
		return nil, fmt.Errorf("volumeMounts exceeds limit %d: %d", m.maxMounts, len(mounts))
	}
	if err := ValidateSegment(sandboxID, "sandbox_id"); err != nil {
		return nil, err
	}
	seenTargets := map[string]struct{}{}
	for _, mount := range mounts {
		target := filepath.Clean(strings.TrimSpace(mount.Path))
		if !filepath.IsAbs(target) {
			return nil, fmt.Errorf("volume mount path must be absolute: %s", mount.Path)
		}
		if isBlockedTarget(target) {
			return nil, fmt.Errorf("volume mount path is not allowed: %s", target)
		}
		if _, ok := seenTargets[target]; ok {
			return nil, fmt.Errorf("duplicate volume mount path: %s", target)
		}
		seenTargets[target] = struct{}{}

		source := filepath.Clean(strings.TrimSpace(mount.Source))
		if source == "" {
			return nil, fmt.Errorf("volume mount source must not be empty")
		}
		if !filepath.IsAbs(source) {
			return nil, fmt.Errorf("volume mount source must be absolute: %s", source)
		}
	}
	return m.backend.Prepare(PrepareRequest{
		Namespace:   namespace,
		SandboxID:   sandboxID,
		Mounts:      mounts,
		OnUnhealthy: onUnhealthy,
	})
}

func (m *Manager) CleanupSandbox(namespace, sandboxID string, devices []Device) error {
	return m.backend.Cleanup(namespace, sandboxID, devices)
}

func (m *Manager) RestoreSandbox(namespace, sandboxID string, devices []Device) error {
	return m.backend.Restore(namespace, sandboxID, devices)
}

func (m *Manager) RestoreSandboxWithHealth(namespace, sandboxID string, devices []Device, onUnhealthy func(error)) error {
	return m.backend.RestoreWithHealth(namespace, sandboxID, devices, onUnhealthy)
}

// CheckSandboxHealth returns a typed backend failure without mutating volume
// state. Sandbox lifecycle code uses it before teardown consumes the record.
func (m *Manager) CheckSandboxHealth(namespace, sandboxID string) error {
	return m.backend.CheckHealth(namespace, sandboxID)
}

// ClearSandboxHealth begins a new observation lifecycle for a sandbox ID.
// Unexpected health remains readable through automatic cleanup until an
// explicit delete or a new create/rehydrate acknowledges the old lifecycle.
func (m *Manager) ClearSandboxHealth(namespace, sandboxID string) {
	m.backend.ClearHealth(namespace, sandboxID)
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
