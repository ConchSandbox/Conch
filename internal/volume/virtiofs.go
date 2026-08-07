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

	socketReadyTimeout  = 3 * time.Second
	processPollInterval = 25 * time.Millisecond
	processStopTimeout  = time.Second
)

var (
	errVirtiofsdExited        = errors.New("virtiofsd process exited")
	errProcessIdentityChanged = errors.New("recorded process identity changed")
)

type sandboxProcessKey struct {
	namespace string
	sandboxID string
}

func newSandboxProcessKey(namespace, sandboxID string) sandboxProcessKey {
	return sandboxProcessKey{namespace: namespace, sandboxID: sandboxID}
}

type virtiofsBackend struct {
	binary     string
	runtimeDir string
	procs      sync.Map
	health     sync.Map
	ops        virtiofsOps
}

type virtiofsOps struct {
	command         func(string, ...string) *exec.Cmd
	mount           func(string, string, string, uintptr, string) error
	unmount         func(string, int) error
	kill            func(*os.Process) error
	processStart    func(int) uint64
	pidfdOpen       func(int, int) (int, error)
	pidfdSendSignal func(int, unix.Signal, *unix.Siginfo, int) error
	poll            func([]unix.PollFd, int) (int, error)
	closeFD         func(int) error
}

func defaultVirtiofsOps() virtiofsOps {
	return virtiofsOps{
		command:         exec.Command,
		mount:           unix.Mount,
		unmount:         unix.Unmount,
		kill:            (*os.Process).Kill,
		processStart:    processStartTicks,
		pidfdOpen:       unix.PidfdOpen,
		pidfdSendSignal: unix.PidfdSendSignal,
		poll:            unix.Poll,
		closeFD:         unix.Close,
	}
}

// virtiofsProcess owns one lifecycle monitor. A child monitor is the sole
// Cmd.Wait caller. An adopted monitor owns a pidfd and never waits a non-child.
type virtiofsProcess struct {
	backend     *virtiofsBackend
	key         sandboxProcessKey
	cmd         *exec.Cmd
	pid         int
	pidfd       int
	adopted     bool
	startTime   uint64
	onUnhealthy func(error)
	done        chan struct{}
	watchStop   chan struct{}

	mu       sync.Mutex
	ready    bool
	stopping bool
	exited   bool
	waitErr  error

	stopOnce sync.Once
	stopErr  error

	watchStopOnce sync.Once
	closeFDOnce   sync.Once
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
		ops:        defaultVirtiofsOps(),
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
	key := newSandboxProcessKey(req.Namespace, req.SandboxID)
	b.health.Delete(key)

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
			if err := b.unmount(binds[i], unix.MNT_DETACH); err != nil {
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
		if err := b.mount(source, bindTarget, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
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

	cmd := b.command(b.binary, b.buildArgs(socket, volumeDir)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("start virtiofsd for sandbox %s: %w", req.SandboxID, err)
	}
	process := &virtiofsProcess{
		backend:     b,
		key:         key,
		cmd:         cmd,
		pid:         cmd.Process.Pid,
		pidfd:       -1,
		startTime:   b.processStart(cmd.Process.Pid),
		onUnhealthy: req.OnUnhealthy,
		done:        make(chan struct{}),
	}
	actual, loaded := b.procs.LoadOrStore(key, process)
	go b.monitorChildProcess(process)
	if loaded {
		stopErr := process.stop()
		cleanup()
		return nil, errors.Join(
			fmt.Errorf("virtiofsd process already tracked for sandbox %s (%T)", req.SandboxID, actual),
			stopErr,
		)
	}
	if err := waitUnixSocket(socket, socketReadyTimeout, process); err != nil {
		stopErr := process.stop()
		cleanup()
		return nil, fmt.Errorf("wait virtiofsd socket %s: %w", socket, errors.Join(err, stopErr))
	}
	if err := process.activate(); err != nil {
		<-process.done
		cleanup()
		return nil, fmt.Errorf("activate virtiofsd for sandbox %s: %w", req.SandboxID, err)
	}

	return []Device{{
		SandboxID:  req.SandboxID,
		Namespace:  req.Namespace,
		Backend:    b.Name(),
		Tag:        virtiofsTag,
		Socket:     socket,
		VolumeDir:  volumeDir,
		ConfigPath: configPath,
		PID:        cmd.Process.Pid,
		StartTime:  process.startTime,
	}}, nil
}

func (b *virtiofsBackend) monitorChildProcess(process *virtiofsProcess) {
	waitErr := process.cmd.Wait()

	process.mu.Lock()
	process.exited = true
	process.waitErr = waitErr
	unexpected := process.ready && !process.stopping
	onUnhealthy := process.onUnhealthy
	unhealthyErr := process.unhealthyErrorLocked()
	process.mu.Unlock()

	// Publish completion before invoking sandbox cleanup. The callback is
	// allowed to synchronously enter Cleanup, which must never wait on itself.
	if unexpected {
		b.health.Store(process.key, unhealthyErr)
	}
	b.procs.CompareAndDelete(process.key, process)
	close(process.done)
	if unexpected && onUnhealthy != nil {
		onUnhealthy(unhealthyErr)
	}
}

func (p *virtiofsProcess) activate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return p.unhealthyErrorLocked()
	}
	p.ready = true
	return nil
}

func (p *virtiofsProcess) unhealthyError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unhealthyErrorLocked()
}

func (p *virtiofsProcess) unhealthyErrorLocked() error {
	cause := p.waitErr
	if cause == nil {
		cause = errVirtiofsdExited
	}
	return &UnhealthyError{
		Backend:   DefaultBackend,
		Namespace: p.key.namespace,
		SandboxID: p.key.sandboxID,
		PID:       p.pid,
		Cause:     cause,
	}
}

func (p *virtiofsProcess) stop() error {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopping = true
		exited := p.exited
		p.mu.Unlock()

		if p.adopted {
			p.watchStopOnce.Do(func() { close(p.watchStop) })
			<-p.done
			p.mu.Lock()
			exited = p.exited
			p.mu.Unlock()
			if !exited {
				if err := p.backend.signalPidfd(p.pidfd); err != nil {
					p.stopErr = fmt.Errorf("kill restored virtiofsd pid %d: %w", p.pid, err)
					p.closePidfd()
					return
				}
				if err := p.backend.waitPidfd(p.pidfd, processStopTimeout); err != nil {
					p.stopErr = fmt.Errorf("wait for restored virtiofsd pid %d exit: %w", p.pid, err)
				}
			}
			p.closePidfd()
			return
		}

		if !exited {
			if err := p.backend.kill(p.cmd.Process); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, os.ErrProcessDone) {
				p.stopErr = fmt.Errorf("kill virtiofsd: %w", err)
				return
			}
		}
		<-p.done
	})
	return p.stopErr
}

func (p *virtiofsProcess) closePidfd() {
	if p.pidfd < 0 {
		return
	}
	p.closeFDOnce.Do(func() {
		_ = p.backend.closeFD(p.pidfd)
	})
}

func (b *virtiofsBackend) command(name string, args ...string) *exec.Cmd {
	if b.ops.command == nil {
		return exec.Command(name, args...)
	}
	return b.ops.command(name, args...)
}

func (b *virtiofsBackend) mount(source, target, fstype string, flags uintptr, data string) error {
	if b.ops.mount == nil {
		return unix.Mount(source, target, fstype, flags, data)
	}
	return b.ops.mount(source, target, fstype, flags, data)
}

func (b *virtiofsBackend) unmount(target string, flags int) error {
	if b.ops.unmount == nil {
		return unix.Unmount(target, flags)
	}
	return b.ops.unmount(target, flags)
}

func (b *virtiofsBackend) kill(process *os.Process) error {
	if b.ops.kill == nil {
		return process.Kill()
	}
	return b.ops.kill(process)
}

func (b *virtiofsBackend) processStart(pid int) uint64 {
	if b.ops.processStart == nil {
		return processStartTicks(pid)
	}
	return b.ops.processStart(pid)
}

func (b *virtiofsBackend) pidfdOpen(pid, flags int) (int, error) {
	if b.ops.pidfdOpen == nil {
		return unix.PidfdOpen(pid, flags)
	}
	return b.ops.pidfdOpen(pid, flags)
}

func (b *virtiofsBackend) pidfdSendSignal(pidfd int, signal unix.Signal, info *unix.Siginfo, flags int) error {
	if b.ops.pidfdSendSignal == nil {
		return unix.PidfdSendSignal(pidfd, signal, info, flags)
	}
	return b.ops.pidfdSendSignal(pidfd, signal, info, flags)
}

func (b *virtiofsBackend) poll(fds []unix.PollFd, timeout int) (int, error) {
	if b.ops.poll == nil {
		return unix.Poll(fds, timeout)
	}
	return b.ops.poll(fds, timeout)
}

func (b *virtiofsBackend) closeFD(fd int) error {
	if b.ops.closeFD == nil {
		return unix.Close(fd)
	}
	return b.ops.closeFD(fd)
}

func (b *virtiofsBackend) buildArgs(socket, volumeDir string) []string {
	// virtiofsd 1.13.x (Rust) has no cache flag; the guest uses the default
	// virtiofs cache mode (see agent mount logic).
	return []string{"--socket-path", socket, "--shared-dir", volumeDir}
}

func (b *virtiofsBackend) Restore(namespace, sandboxID string, devices []Device) error {
	return b.restore(namespace, sandboxID, devices, nil)
}

func (b *virtiofsBackend) RestoreWithHealth(namespace, sandboxID string, devices []Device, onUnhealthy func(error)) error {
	return b.restore(namespace, sandboxID, devices, onUnhealthy)
}

func (b *virtiofsBackend) restore(namespace, sandboxID string, devices []Device, onUnhealthy func(error)) error {
	if len(devices) != 1 {
		return fmt.Errorf("expected one virtiofs device, got %d", len(devices))
	}
	key := newSandboxProcessKey(namespace, sandboxID)
	b.health.Delete(key)
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
	pidfd, err := b.openRecordedProcess(device.PID, device.StartTime)
	if err != nil {
		return fmt.Errorf("restore virtiofsd pid %d: %w", device.PID, err)
	}
	process := &virtiofsProcess{
		backend:     b,
		key:         key,
		pid:         device.PID,
		pidfd:       pidfd,
		adopted:     true,
		startTime:   device.StartTime,
		onUnhealthy: onUnhealthy,
		done:        make(chan struct{}),
		watchStop:   make(chan struct{}),
		ready:       true,
	}
	if actual, loaded := b.procs.LoadOrStore(key, process); loaded {
		process.closePidfd()
		return fmt.Errorf("virtiofsd process already tracked for sandbox %s (%T)", sandboxID, actual)
	}
	go b.monitorRestoredProcess(process)
	return nil
}

func (b *virtiofsBackend) monitorRestoredProcess(process *virtiofsProcess) {
	for {
		select {
		case <-process.watchStop:
			close(process.done)
			return
		default:
		}

		fds := []unix.PollFd{{Fd: int32(process.pidfd), Events: unix.POLLIN}}
		n, err := b.poll(fds, int(processPollInterval.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			b.finishRestoredMonitorFailure(process, fmt.Errorf("poll pidfd: %w", err))
			return
		}
		if n == 0 {
			continue
		}
		revents := fds[0].Revents
		if revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			b.finishRestoredExit(process)
			return
		}
		if revents&unix.POLLNVAL != 0 {
			b.finishRestoredMonitorFailure(process, fmt.Errorf("poll pidfd returned POLLNVAL"))
			return
		}
	}
}

func (b *virtiofsBackend) finishRestoredExit(process *virtiofsProcess) {
	process.mu.Lock()
	process.exited = true
	process.waitErr = errVirtiofsdExited
	unexpected := !process.stopping
	onUnhealthy := process.onUnhealthy
	unhealthyErr := process.unhealthyErrorLocked()
	process.mu.Unlock()

	if unexpected {
		b.health.Store(process.key, unhealthyErr)
		b.procs.CompareAndDelete(process.key, process)
	}
	close(process.done)
	if unexpected {
		process.closePidfd()
		if onUnhealthy != nil {
			onUnhealthy(unhealthyErr)
		}
	}
}

func (b *virtiofsBackend) finishRestoredMonitorFailure(process *virtiofsProcess, monitorErr error) {
	process.mu.Lock()
	process.waitErr = monitorErr
	unexpected := !process.stopping
	onUnhealthy := process.onUnhealthy
	unhealthyErr := process.unhealthyErrorLocked()
	process.mu.Unlock()

	if unexpected {
		// Keep the live record and pidfd so Cleanup can still terminate the
		// original process safely after a watcher failure.
		b.health.Store(process.key, unhealthyErr)
	}
	close(process.done)
	if unexpected && onUnhealthy != nil {
		onUnhealthy(unhealthyErr)
	}
}

func (b *virtiofsBackend) openRecordedProcess(pid int, startTime uint64) (int, error) {
	if !b.isOurVirtiofsd(pid, startTime) {
		return -1, fmt.Errorf("%w: pid/start-time identity is not current", errProcessIdentityChanged)
	}
	pidfd, err := b.pidfdOpen(pid, 0)
	if err != nil {
		return -1, fmt.Errorf("open pidfd: %w", err)
	}
	// Revalidate after opening. If the original exited between the first
	// /proc read and pidfd_open, the fd may identify a reused PID; never adopt
	// or signal it when the recorded start time no longer matches.
	if !b.isOurVirtiofsd(pid, startTime) {
		_ = b.closeFD(pidfd)
		return -1, fmt.Errorf("%w while opening pidfd", errProcessIdentityChanged)
	}
	return pidfd, nil
}

func (b *virtiofsBackend) isOurVirtiofsd(pid int, startTime uint64) bool {
	if pid <= 0 || startTime == 0 {
		return false
	}
	current := b.processStart(pid)
	return current != 0 && current == startTime
}

func (b *virtiofsBackend) signalPidfd(pidfd int) error {
	if err := b.pidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

func (b *virtiofsBackend) waitPidfd(pidfd int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out after %s", timeout)
		}
		wait := min(remaining, processPollInterval)
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		n, err := b.poll(fds, max(1, int(wait.Milliseconds())))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll pidfd: %w", err)
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			return nil
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return fmt.Errorf("poll pidfd returned POLLNVAL")
		}
	}
}

func (b *virtiofsBackend) killRecordedProcess(pid int, startTime uint64) error {
	pidfd, err := b.openRecordedProcess(pid, startTime)
	if err != nil {
		// A missing or changed identity means the recorded process is already
		// gone. It is convergence, not permission to signal the current PID.
		if errors.Is(err, errProcessIdentityChanged) {
			return nil
		}
		return err
	}
	defer b.closeFD(pidfd)
	if err := b.signalPidfd(pidfd); err != nil {
		return err
	}
	return b.waitPidfd(pidfd, processStopTimeout)
}

// CheckHealth returns the retained unexpected-exit error for a sandbox. It is
// read-only; automatic Cleanup preserves the record until explicit deletion
// or the next create/rehydrate begins a new lifecycle for the sandbox ID.
func (b *virtiofsBackend) CheckHealth(namespace, sandboxID string) error {
	value, ok := b.health.Load(newSandboxProcessKey(namespace, sandboxID))
	if !ok {
		return nil
	}
	healthErr, ok := value.(error)
	if !ok {
		return fmt.Errorf("invalid volume health state for sandbox %s", sandboxID)
	}
	return healthErr
}

func (b *virtiofsBackend) ClearHealth(namespace, sandboxID string) {
	b.health.Delete(newSandboxProcessKey(namespace, sandboxID))
}

func (b *virtiofsBackend) Cleanup(namespace, sandboxID string, devices []Device) error {
	var errs []error
	key := newSandboxProcessKey(namespace, sandboxID)
	healthErr := b.CheckHealth(namespace, sandboxID)
	retryRecordedProcess := false
	managedProcess := false

	if v, ok := b.procs.Load(key); ok {
		if process, ok := v.(*virtiofsProcess); ok {
			managedProcess = true
			if stopErr := process.stop(); stopErr != nil {
				errs = append(errs, stopErr)
				retryRecordedProcess = true
			}
			// The monitor has either joined or remains the sole Wait owner.
			// Drop lifecycle ownership even on a signaling error so a later
			// Cleanup can safely retry through the recorded PID/start-time path.
			b.procs.CompareAndDelete(key, process)
		}
	}
	if !managedProcess && healthErr == nil {
		healthErr = b.CheckHealth(namespace, sandboxID)
	}
	if retryRecordedProcess || (!managedProcess && healthErr == nil) {
		for _, device := range devices {
			if device.PID <= 0 {
				continue
			}
			if killErr := b.killRecordedProcess(device.PID, device.StartTime); killErr != nil {
				errs = append(errs, fmt.Errorf("kill virtiofsd pid %d: %w", device.PID, killErr))
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
			if umountErr := b.unmount(p, unix.MNT_DETACH); umountErr != nil && !errors.Is(umountErr, unix.EINVAL) {
				errs = append(errs, fmt.Errorf("unmount %s: %w", p, umountErr))
			}
		}
	}
	if rmErr := os.RemoveAll(runtimeDir); rmErr != nil {
		errs = append(errs, fmt.Errorf("remove runtime dir %s: %w", runtimeDir, rmErr))
	}
	// Preserve unexpected health through automatic sandbox cleanup so later
	// lifecycle/diagnostic calls can report the typed cause. Expected cleanup
	// clears any prior state; explicit delete or a new lifecycle also clears it.
	// Re-read after stop/join: an exit may have won the stopping mutex just
	// after the initial lookup and published health before closing done.
	if b.CheckHealth(namespace, sandboxID) == nil {
		b.health.Delete(key)
	}
	return errors.Join(errs...)
}

// processStartTicks reads /proc/<pid>/stat field 22 (starttime in clock ticks
// since boot). Returns 0 if the process was reaped or the file is unreadable.
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

func waitUnixSocket(path string, timeout time.Duration, process *virtiofsProcess) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		st, err := os.Stat(path)
		if err == nil && (st.Mode()&os.ModeSocket) != 0 {
			return nil
		}
		lastErr = err
		select {
		case <-process.done:
			return process.unhealthyError()
		case <-timer.C:
			if lastErr != nil {
				return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
			}
			return fmt.Errorf("timed out after %s waiting for unix socket", timeout)
		case <-ticker.C:
		}
	}
}
