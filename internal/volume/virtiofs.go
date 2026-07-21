package volume

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"github.com/moby/sys/mountinfo"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	DefaultBackend    = "virtiofs"
	DefaultRuntimeDir = "/run/conch/sandboxes"
	DefaultBinary     = "virtiofsd"

	volumeDirName  = "volume"
	socketName     = "virtiofs.sock"
	configFileName = "config.json"
	virtiofsTag    = "conchfs"
	configVersion  = 1

	socketReadyTimeout = 3 * time.Second
)

type virtiofsBackend struct {
	binary     string
	runtimeDir string
	procs      sync.Map
}

func NewVirtiofsBackend(cfg VirtiofsConfig) Backend {
	if strings.TrimSpace(cfg.Binary) == "" {
		cfg.Binary = DefaultBinary
	}
	if strings.TrimSpace(cfg.RuntimeDir) == "" {
		cfg.RuntimeDir = DefaultRuntimeDir
	}
	return &virtiofsBackend{
		binary:     cfg.Binary,
		runtimeDir: filepath.Clean(cfg.RuntimeDir),
	}
}

func (b *virtiofsBackend) Name() string {
	return DefaultBackend
}

type configDocument struct {
	Version int           `json:"version"`
	Mounts  []configMount `json:"mounts"`
}

type configMount struct {
	Index    int    `json:"index"`
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

func (b *virtiofsBackend) Prepare(req PrepareRequest) ([]Device, error) {
	if len(req.Mounts) == 0 {
		return nil, nil
	}
	if err := ValidateSegment(req.SandboxID, "sandbox_id"); err != nil {
		return nil, err
	}

	runtimeDir := filepath.Join(b.runtimeDir, req.SandboxID)
	volumeDir := filepath.Join(runtimeDir, volumeDirName)
	socket := filepath.Join(runtimeDir, socketName)
	configPath := filepath.Join(volumeDir, configFileName)

	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create volume dir: %w", err)
	}

	var binds []string
	cleanup := func() {
		var umountErr = false
		for i := len(binds) - 1; i >= 0; i-- {
			if err := unix.Unmount(binds[i], unix.MNT_DETACH); err != nil {
				umountErr = true
				ulog.Warn("failed umount", ulog.F("bind", binds[i]))
			}
		}
		if umountErr {
			ulog.Warn("umount error occurred, skip remove", ulog.F("runtimeDir", runtimeDir))
		} else {
			_ = os.RemoveAll(runtimeDir)
		}
	}

	for i, mount := range req.Mounts {
		source := filepath.Clean(mount.Source)
		if _, err := os.Stat(source); err != nil {
			cleanup()
			return nil, fmt.Errorf("volume source not accessible: %s: %w", source, err)
		}
		bindTarget := filepath.Join(volumeDir, strconv.Itoa(i))
		if err := os.MkdirAll(bindTarget, 0o755); err != nil {
			cleanup()
			return nil, fmt.Errorf("create bind target: %w", err)
		}
		if err := unix.Mount(source, bindTarget, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			cleanup()
			return nil, fmt.Errorf("bind volume source %s: %w", source, err)
		}
		binds = append(binds, bindTarget)
	}

	doc := configDocument{Version: configVersion}
	for i, mount := range req.Mounts {
		doc.Mounts = append(doc.Mounts, configMount{
			Index:    i,
			Path:     mount.Path,
			Readonly: mount.Readonly,
		})
	}
	data, err := json.Marshal(doc)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("marshal config.json: %w", err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		cleanup()
		return nil, fmt.Errorf("write config.json: %w", err)
	}

	cmd := exec.Command(b.binary, b.buildArgs(socket, volumeDir)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("start virtiofsd for sandbox %s: %w", req.SandboxID, err)
	}
	if err := waitUnixSocket(socket, socketReadyTimeout); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		cleanup()
		return nil, fmt.Errorf("wait virtiofsd socket %s: %w", socket, err)
	}
	b.procs.Store(req.SandboxID, cmd)

	return []Device{{
		SandboxID:  req.SandboxID,
		Namespace:  req.Namespace,
		Backend:    b.Name(),
		Tag:        virtiofsTag,
		Socket:     socket,
		VolumeDir:  volumeDir,
		ConfigPath: configPath,
		PID:        cmd.Process.Pid,
		StartTime:  processStartTicks(cmd.Process.Pid),
	}}, nil
}

func (b *virtiofsBackend) buildArgs(socket, volumeDir string) []string {
	// virtiofsd 1.13.x (Rust) has no cache flag; the guest uses the default
	// virtiofs cache mode (see agent mount logic).
	return []string{"--socket-path", socket, "--shared-dir", volumeDir}
}

func (b *virtiofsBackend) Restore(namespace, sandboxID string, devices []Device) error {
	if len(devices) != 1 {
		return fmt.Errorf("expected one virtiofs device, got %d", len(devices))
	}
	device := devices[0]
	if device.Backend != "" && device.Backend != b.Name() {
		return fmt.Errorf("unsupported restored volume backend %q", device.Backend)
	}
	if device.SandboxID != "" && device.SandboxID != sandboxID {
		return fmt.Errorf("restored volume sandbox_id mismatch: got %q want %q", device.SandboxID, sandboxID)
	}
	if strings.TrimSpace(device.Socket) == "" {
		return fmt.Errorf("restored virtiofs socket is empty")
	}
	st, err := os.Stat(device.Socket)
	if err != nil {
		return fmt.Errorf("stat restored virtiofs socket %s: %w", device.Socket, err)
	}
	if st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("restored virtiofs socket %s is not a unix socket", device.Socket)
	}
	if strings.TrimSpace(device.VolumeDir) == "" {
		return fmt.Errorf("restored virtiofs volume dir is empty")
	}
	if st, err := os.Stat(device.VolumeDir); err != nil {
		return fmt.Errorf("stat restored virtiofs volume dir %s: %w", device.VolumeDir, err)
	} else if !st.IsDir() {
		return fmt.Errorf("restored virtiofs volume dir %s is not a directory", device.VolumeDir)
	}
	if strings.TrimSpace(device.ConfigPath) == "" {
		return fmt.Errorf("restored virtiofs config path is empty")
	}
	data, err := os.ReadFile(device.ConfigPath)
	if err != nil {
		return fmt.Errorf("read restored virtiofs config %s: %w", device.ConfigPath, err)
	}
	var doc configDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse restored virtiofs config %s: %w", device.ConfigPath, err)
	}
	if doc.Version != configVersion {
		return fmt.Errorf("unsupported restored virtiofs config version %d", doc.Version)
	}
	for _, mount := range doc.Mounts {
		if mount.Index < 0 {
			return fmt.Errorf("restored virtiofs config has negative mount index %d", mount.Index)
		}
		bindTarget := filepath.Join(device.VolumeDir, strconv.Itoa(mount.Index))
		if ok, err := isMountPoint(bindTarget); err != nil {
			return fmt.Errorf("check restored bind mount %s: %w", bindTarget, err)
		} else if !ok {
			return fmt.Errorf("restored bind target %s is not a mount point", bindTarget)
		}
	}
	if !isOurVirtiofsd(device.PID, device.StartTime) {
		return fmt.Errorf("restored virtiofsd pid %d does not match recorded process", device.PID)
	}
	return nil
}

func (b *virtiofsBackend) Cleanup(namespace, sandboxID string, devices []Device) error {
	var errs []error

	if v, ok := b.procs.LoadAndDelete(sandboxID); ok {
		if cmd, ok := v.(*exec.Cmd); ok && cmd.Process != nil {
			if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, unix.ESRCH) {
				errs = append(errs, fmt.Errorf("kill virtiofsd: %w", killErr))
			}
			_, _ = cmd.Process.Wait()
		}
	} else {
		for _, device := range devices {
			if device.PID <= 0 {
				continue
			}
			if !isOurVirtiofsd(device.PID, device.StartTime) {
				continue
			}
			if proc, err := os.FindProcess(device.PID); err == nil {
				if killErr := proc.Kill(); killErr != nil && !errors.Is(killErr, unix.ESRCH) {
					errs = append(errs, fmt.Errorf("kill virtiofsd pid %d: %w", device.PID, killErr))
				}
			}
		}
	}

	var volumeDir, runtimeDir string
	for _, device := range devices {
		if device.VolumeDir != "" {
			volumeDir = device.VolumeDir
			break
		}
	}
	if volumeDir == "" {
		runtimeDir = filepath.Join(b.runtimeDir, sandboxID)
		volumeDir = filepath.Join(runtimeDir, volumeDirName)
	} else {
		runtimeDir = filepath.Dir(volumeDir)
	}

	if entries, err := os.ReadDir(volumeDir); err == nil {
		for _, entry := range entries {
			p := filepath.Join(volumeDir, entry.Name())
			if umountErr := unix.Unmount(p, unix.MNT_DETACH); umountErr != nil && !errors.Is(umountErr, unix.EINVAL) {
				errs = append(errs, fmt.Errorf("unmount %s: %w", p, umountErr))
			}
		}
	}
	if rmErr := os.RemoveAll(runtimeDir); rmErr != nil {
		errs = append(errs, fmt.Errorf("remove runtime dir %s: %w", runtimeDir, rmErr))
	}
	return errors.Join(errs...)
}

// processStartTicks reads /proc/<pid>/stat field 22 (starttime in clock ticks
// since boot). Returns 0 if the process has exited or the file is unreadable.
func processStartTicks(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 || idx+2 >= len(data) {
		return 0
	}
	rest := strings.Fields(string(data)[idx+2:])
	if len(rest) < 20 {
		return 0
	}
	val, err := strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		return 0
	}
	return val
}

// isOurVirtiofsd returns true only if the PID still exists and its starttime
// matches the recorded value. A mismatch means the original virtiofsd exited
// and the PID was reused by an unrelated process — must NOT kill.
func isOurVirtiofsd(pid int, startTime uint64) bool {
	if pid <= 0 || startTime == 0 {
		return false
	}
	current := processStartTicks(pid)
	if current == 0 {
		return false
	}
	return current == startTime
}

func isMountPoint(path string) (bool, error) {
	return mountinfo.Mounted(path)
}

func waitUnixSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		st, err := os.Stat(path)
		if err == nil && (st.Mode()&os.ModeSocket) != 0 {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
			}
			return fmt.Errorf("timed out after %s waiting for unix socket", timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
