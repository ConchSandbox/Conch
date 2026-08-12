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

	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"

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

func (b *virtiofsBackend) Prepare(req PrepareRequest) (PreparedSandbox, error) {
	if len(req.Mounts) == 0 {
		return PreparedSandbox{}, nil
	}
	if err := ValidateSegment(req.SandboxID, "sandbox_id"); err != nil {
		return PreparedSandbox{}, err
	}

	runtimeDir := filepath.Join(b.runtimeDir, req.SandboxID)
	volumeDir := filepath.Join(runtimeDir, volumeDirName)
	socket := filepath.Join(runtimeDir, socketName)
	configPath := filepath.Join(volumeDir, configFileName)

	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		return PreparedSandbox{}, fmt.Errorf("create volume dir: %w", err)
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
			return PreparedSandbox{}, fmt.Errorf("volume source not accessible: %s: %w", source, err)
		}
		bindTarget := filepath.Join(volumeDir, strconv.Itoa(i))
		if err := os.MkdirAll(bindTarget, 0o755); err != nil {
			cleanup()
			return PreparedSandbox{}, fmt.Errorf("create bind target: %w", err)
		}
		if err := unix.Mount(source, bindTarget, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			cleanup()
			return PreparedSandbox{}, fmt.Errorf("bind volume source %s: %w", source, err)
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
		return PreparedSandbox{}, fmt.Errorf("marshal config.json: %w", err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		cleanup()
		return PreparedSandbox{}, fmt.Errorf("write config.json: %w", err)
	}

	cmd := exec.Command(b.binary, b.buildArgs(socket, volumeDir)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	process, err := b.startVirtiofsProcess(req.SandboxID, cmd, socket, socketReadyTimeout)
	if err != nil {
		cleanup()
		return PreparedSandbox{}, err
	}

	devices := []Device{{
		SandboxID:  req.SandboxID,
		Backend:    b.Name(),
		Tag:        virtiofsTag,
		Socket:     socket,
		VolumeDir:  volumeDir,
		ConfigPath: configPath,
		PID:        process.handle.PID(),
		StartTime:  process.handle.StartTime(),
	}}
	return PreparedSandbox{Devices: devices, Watch: &ProcessWatch{process: process}}, nil
}

func (b *virtiofsBackend) startVirtiofsProcess(
	sandboxID string,
	cmd *exec.Cmd,
	socket string,
	timeout time.Duration,
) (*virtiofsProcess, error) {
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start virtiofsd for sandbox %s: %w", sandboxID, err)
	}
	child, err := newChildProcess(cmd, processStartTicks(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, err
	}
	process := newVirtiofsProcess(sandboxID, child)
	if _, loaded := b.procs.LoadOrStore(sandboxID, process); loaded {
		// This child was just started, so it still needs its unique reaper even
		// though another owner already occupies the sandbox key.
		go b.monitorProcess(process)
		closeErr := process.Close()
		return nil, errors.Join(
			fmt.Errorf("virtiofsd process for sandbox %s already exists", sandboxID),
			closeErr,
		)
	}
	go b.monitorProcess(process)

	if socketErr := waitUnixSocket(socket, timeout, process.Done()); socketErr != nil {
		var startupErr error
		if result, ok := process.Result(); ok {
			startupErr = observationError("virtiofsd exited while waiting for its socket", result)
		}
		closeErr := process.Close()
		b.procs.CompareAndDelete(sandboxID, process)
		if startupErr != nil {
			return nil, errors.Join(startupErr, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("wait virtiofsd socket %s: %w", socket, socketErr), closeErr)
	}
	if err := process.markPrepared(); err != nil {
		closeErr := process.Close()
		b.procs.CompareAndDelete(sandboxID, process)
		return nil, errors.Join(err, closeErr)
	}
	return process, nil
}

func (b *virtiofsBackend) monitorProcess(process *virtiofsProcess) {
	wait := process.handle.Wait()
	process.storeObservation(wait)
	b.procs.CompareAndDelete(process.sandboxID, process)
	close(process.observeDone)
}

func (b *virtiofsBackend) Adopt(sandboxID string, devices []Device) (PreparedSandbox, error) {
	if len(devices) == 0 {
		return PreparedSandbox{}, nil
	}
	if len(devices) != 1 {
		return PreparedSandbox{}, fmt.Errorf("sandbox %s has %d virtiofs devices, want 1", sandboxID, len(devices))
	}
	device := devices[0]
	if device.SandboxID != "" && device.SandboxID != sandboxID {
		return PreparedSandbox{}, fmt.Errorf("virtiofs device belongs to sandbox %s, not %s", device.SandboxID, sandboxID)
	}
	handle, err := newAdoptedProcess(device.PID, device.StartTime)
	if err != nil {
		return PreparedSandbox{}, err
	}
	process := newVirtiofsProcess(sandboxID, handle)
	if _, loaded := b.procs.LoadOrStore(sandboxID, process); loaded {
		return PreparedSandbox{}, errors.Join(
			fmt.Errorf("virtiofsd process for sandbox %s already exists", sandboxID),
			handle.Close(),
		)
	}
	go b.monitorProcess(process)
	if err := process.markPrepared(); err != nil {
		closeErr := process.Close()
		b.procs.CompareAndDelete(sandboxID, process)
		return PreparedSandbox{}, errors.Join(err, closeErr)
	}
	return PreparedSandbox{
		Devices: append([]Device(nil), devices...),
		Watch:   &ProcessWatch{process: process},
	}, nil
}

func (b *virtiofsBackend) buildArgs(socket, volumeDir string) []string {
	// virtiofsd 1.13.x (Rust) has no cache flag; the guest uses the default
	// virtiofs cache mode (see agent mount logic).
	return []string{"--socket-path", socket, "--shared-dir", volumeDir}
}

func (b *virtiofsBackend) Cleanup(sandboxID string, prepared PreparedSandbox) error {
	var errs []error
	if prepared.Watch != nil && prepared.Watch.process != nil {
		process := prepared.Watch.process
		b.procs.CompareAndDelete(sandboxID, process)
		if closeErr := process.Close(); closeErr != nil {
			errs = append(errs, closeErr)
		}
	} else {
		// A zero Watch is not an active owner. Persisted/stale device cleanup
		// still binds a pidfd before signaling so PID reuse cannot be raced.
		for _, device := range prepared.Devices {
			if terminateErr := terminateAdoptedProcess(device.PID, device.StartTime); terminateErr != nil {
				errs = append(errs, terminateErr)
			}
		}
	}

	if resourceErr := b.cleanupVolumeResources(sandboxID, prepared.Devices); resourceErr != nil {
		errs = append(errs, resourceErr)
	}
	return errors.Join(errs...)
}

func (b *virtiofsBackend) cleanupVolumeResources(sandboxID string, devices []Device) error {
	var errs []error
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

func (b *virtiofsBackend) CleanupStaleResources() error {
	var errs []error
	procEntries, procErr := os.ReadDir("/proc")
	if procErr != nil {
		return fmt.Errorf("scan virtiofsd processes: %w", procErr)
	}
	for _, entry := range procEntries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || !strings.Contains(string(cmdline), "virtiofsd") || !strings.Contains(string(cmdline), b.runtimeDir) {
			continue
		}
		pid, _ := strconv.Atoi(entry.Name())
		if terminateErr := terminateAdoptedProcess(pid, processStartTicks(pid)); terminateErr != nil {
			errs = append(errs, fmt.Errorf("terminate stale virtiofsd pid %d: %w", pid, terminateErr))
		}
	}
	entries, err := os.ReadDir(b.runtimeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.Join(errs...)
		}
		return errors.Join(append(errs, err)...)
	}
	for _, entry := range entries {
		if !entry.IsDir() || ValidateSegment(entry.Name(), "sandbox_id") != nil {
			continue
		}
		if cleanupErr := b.cleanupVolumeResources(entry.Name(), nil); cleanupErr != nil {
			errs = append(errs, fmt.Errorf("cleanup stale volume for sandbox %s: %w", entry.Name(), cleanupErr))
		}
	}
	return errors.Join(errs...)
}

func terminateAdoptedProcess(pid int, startTime uint64) error {
	if !isOurVirtiofsd(pid, startTime) {
		return nil
	}
	handle, err := newAdoptedProcess(pid, startTime)
	if err != nil {
		// Identity loss means the original process is already gone or the PID
		// was reused; either case must converge without signaling the new PID.
		if !isOurVirtiofsd(pid, startTime) {
			return nil
		}
		return fmt.Errorf("bind pidfd for virtiofsd pid %d: %w", pid, err)
	}
	var errs []error
	if killErr := handle.Kill(); killErr != nil {
		errs = append(errs, fmt.Errorf("kill virtiofsd pid %d: %w", pid, killErr))
	}
	if confirmErr := handle.ConfirmExit(processCloseTimeout); confirmErr != nil {
		errs = append(errs, fmt.Errorf("confirm virtiofsd pid %d exit: %w", pid, confirmErr))
	}
	if closeErr := handle.Close(); closeErr != nil {
		errs = append(errs, fmt.Errorf("close virtiofsd pid %d handle: %w", pid, closeErr))
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

func waitUnixSocket(path string, timeout time.Duration, processDone <-chan struct{}) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var lastErr error
	for {
		st, err := os.Stat(path)
		if err == nil && (st.Mode()&os.ModeSocket) != 0 {
			return nil
		}
		lastErr = err
		select {
		case <-processDone:
			return fmt.Errorf("virtiofsd exited before socket became ready")
		case <-ticker.C:
		case <-timer.C:
			if lastErr != nil {
				return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
			}
			return fmt.Errorf("timed out after %s waiting for unix socket", timeout)
		}
	}
}
