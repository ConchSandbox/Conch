// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: PID 1 initialization logic for conch-init

package guestd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	// MergeTarget is the OverlayFS merge point
	MergeTarget      = "/mnt/conch/merge"
	initLogPath      = "/var/log/conch-init/conch-init.log"
	initMergeLogPath = MergeTarget + initLogPath
)

type initDir struct {
	path string
	mode os.FileMode
}

type initDevice struct {
	path  string
	mode  uint32
	major uint32
	minor uint32
}

func ensureProcMounted() {
	logger := ulog.GetLogger()
	if isMountPoint("/proc") {
		return
	}

	if err := os.MkdirAll("/proc", 0755); err != nil {
		logger.Warn("Failed to create /proc before bootstrap mount", ulog.F("error", err))
		return
	}

	if err := syscall.Mount("none", "/proc", "proc", 0, ""); err != nil {
		logger.Warn("Failed to bootstrap mount /proc", ulog.F("error", err))
	}
}

func ensureInitrdRuntimeLayout() {
	logger := ulog.GetLogger()
	dirs := []initDir{
		{"/proc", 0755},
		{"/sys", 0755},
		{"/dev", 0755},
		{"/dev/pts", 0755},
		{"/run", 0755},
		{"/tmp", 01777},
		{"/var", 0755},
		{"/var/log", 0755},
		{"/var/log/conch-init", 0755},
		{"/mnt", 0755},
		{"/mnt/conch", 0755},
		{"/mnt/conch/upper", 0755},
		{"/mnt/conch/work", 0755},
		{MergeTarget, 0755},
		{"/mnt/disk", 0755},
		{"/etc", 0755},
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir.path, dir.mode); err != nil {
			logger.Warn("Failed to create initrd directory", ulog.F("path", dir.path), ulog.F("error", err))
			continue
		}
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			logger.Warn("Failed to chmod initrd directory", ulog.F("path", dir.path), ulog.F("error", err))
		}
	}
}

// createDeviceNodes creates essential device nodes before mounting devtmpfs.
func createDeviceNodes() {
	logger := ulog.GetLogger()
	devices := []initDevice{
		{"/dev/null", 0666, 1, 3},
		{"/dev/zero", 0666, 1, 5},
		{"/dev/full", 0666, 1, 7},
		{"/dev/random", 0666, 1, 8},
		{"/dev/urandom", 0666, 1, 9},
		{"/dev/tty", 0666, 5, 0},
		{"/dev/console", 0600, 5, 1},
	}

	if err := os.MkdirAll("/dev", 0755); err != nil {
		logger.Warn("Failed to create /dev", ulog.F("error", err))
		return
	}
	for _, dev := range devices {
		mode := dev.mode | syscall.S_IFCHR
		if err := syscall.Mknod(dev.path, mode, int(unix.Mkdev(dev.major, dev.minor))); err != nil && !errors.Is(err, syscall.EEXIST) {
			logger.Warn("Failed to create device node", ulog.F("path", dev.path), ulog.F("error", err))
		}
	}
}

func setupInitFileLogging() {
	initDefaultLogger("")
	refreshSandboxLoggerFromCmdline()
	ulog.GetLogger().Info("Using console logging before rootfs log is available")
}

func setupMergeFileLogging() {
	initDefaultLogger(initMergeLogPath)
	refreshSandboxLoggerFromCmdline()
	ulog.GetLogger().Info("Using rootfs log file", ulog.F("path", initLogPath))
}

// runAsInit runs conch-init as PID 1 (init process)
func runAsInit() {
	os.Setenv("PATH", "/sbin:/bin:/usr/sbin:/usr/bin")
	ensureProcMounted()
	sandboxID := refreshSandboxLoggerFromCmdline()
	fields := []ulog.Field{ulog.F("pid", os.Getpid())}
	if sandboxID != "" {
		fields = append(fields, ulog.F("sandbox_id", sandboxID))
	}
	ulog.GetLogger().Info("Starting conch-init as init process", fields...)

	ensureInitrdRuntimeLayout()
	createDeviceNodes()
	mountEssentialFilesystems()
	setupInitFileLogging()
	mountStorageDevices()

	mergeReady := false
	if _, err := os.Stat(MergeTarget + "/usr"); err == nil {
		mergeReady = true
	}

	if mergeReady {
		markRootfsMergeReady()
		prepareMergeRoot()
		bindMountToMerge()
		mountConfiguredVolumesOrAbort()
		setupDevPts()
		setupMergeFileLogging()

		rootfsEntrypointExpected.Store(hasRootfsEntrypoint())
		if err := chrootToMerge(); err != nil {
			ulog.GetLogger().Error("Failed to chroot into merge layer, aborting init")
			return
		}
	} else {
		ulog.GetLogger().Warn("Overlay rootfs not found", ulog.F("target", MergeTarget))
	}

	handler := NewVsockHandler(checkSandboxReady, nil)
	vsockServerErr, err := startVsockServer(handler)
	if err != nil {
		ulog.GetLogger().Error("Failed to start vsock initialization server", ulog.F("error", err))
		return
	}

	select {
	case <-handler.NetworkReady():
	case err := <-vsockServerErr:
		ulog.GetLogger().Error("vsock server stopped before network initialization", ulog.F("error", err))
		return
	}

	go waitForRootfsServiceReadySignal()
	if rootfsEntrypointExpected.Load() {
		ulog.GetLogger().Info("Rootfs conch entrypoint found; using rootfs service startup")
		startRootfsEntrypoint()
	} else {
		ulog.GetLogger().Info("Rootfs conch entrypoint not found; skipping rootfs service startup")
	}

	if err := startAgentAPIServerAsync(); err != nil {
		ulog.GetLogger().Error("agent API server failed to start, vsock will report NOT_READY",
			ulog.F("error", err),
		)
	}

	go reapChildren()

	ulog.GetLogger().Info("Initialization complete")
	waitForSignal(vsockServerErr)
}

// chrootToMerge chroot to the OverlayFS merge layer.
func chrootToMerge() error {
	logger := ulog.GetLogger()
	if err := syscall.Chroot(MergeTarget); err != nil {
		logger.Error("Chroot failed", ulog.F("target", MergeTarget), ulog.F("error", err))
		return err
	}
	if err := os.Chdir("/"); err != nil {
		logger.Error("Chdir failed", ulog.F("target", "/"), ulog.F("error", err))
		return err
	}
	return nil
}

func waitForRootfsServiceReadySignal() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	for range sigCh {
		markRootfsServicesReady()
		ulog.GetLogger().Info("Rootfs services marked ready via SIGUSR1")
	}
}

// waitForSignal waits for SIGTERM/SIGINT or a fatal vsock server error.
func waitForSignal(vsockServerErr <-chan error) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sigCh:
		ulog.GetLogger().Info("Received shutdown signal")
	case err := <-vsockServerErr:
		ulog.GetLogger().Error("vsock server stopped", ulog.F("error", err))
	}
}

// startAgentAPIServerAsync binds the agent API listener synchronously, then serves in
// the background. Returning after net.Listen succeeds makes vsock READY checks
// deterministic without waiting on rootfs services.
func startAgentAPIServerAsync() error {
	logger := ulog.GetLogger()
	listener, err := net.Listen("tcp", ServerPort)
	if err != nil {
		logger.Error("Failed to listen on agent API port",
			ulog.F("port", ServerPort),
			ulog.F("error", err),
		)
		return err
	}

	logger.Info("agent API server listening", ulog.F("port", ServerPort))
	markAgentAPIReady()
	go func() {
		if err := serveAgentAPI(listener); err != nil {
			markAgentAPINotReady()
			ulog.GetLogger().Error("agent API server error", ulog.F("error", err))
		}
	}()
	return nil
}

// startVsockServer creates the listener synchronously, then serves connections
// in the background. The returned channel reports fatal listener errors.
func startVsockServer(handler VsockHandler) (<-chan error, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("create vsock socket: %w", err)
	}

	sa := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY,
		Port: uint32(vsockReadyPort),
	}

	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind vsock socket: %w", err)
	}

	if err := unix.Listen(fd, 5); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("listen on vsock socket: %w", err)
	}

	ulog.GetLogger().Info("vsock server listening", ulog.F("port", vsockReadyPort))
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- serveVsock(fd, handler)
	}()
	return serverErr, nil
}

func serveVsock(fd int, handler VsockHandler) error {
	defer unix.Close(fd)
	logger := ulog.GetLogger()
	for {
		connFd, _, err := unix.Accept(fd)
		if err != nil {
			if errors.Is(err, unix.EINTR) ||
				errors.Is(err, unix.EAGAIN) ||
				errors.Is(err, unix.ECONNABORTED) {
				continue
			}
			return fmt.Errorf("accept vsock connection: %w", err)
		}

		if err := handleVsockConnection(connFd, handler); err != nil {
			logger.Warn("failed to handle vsock connection", ulog.F("error", err))
		}
	}
}

func handleVsockConnection(connFd int, handler VsockHandler) error {
	file := os.NewFile(uintptr(connFd), "guest-vsock")
	if file == nil {
		_ = unix.Close(connFd)
		return fmt.Errorf("wrap vsock fd %d", connFd)
	}
	defer file.Close()

	tv := unix.NsecToTimeval(int64(2 * time.Second))
	_ = unix.SetsockoptTimeval(connFd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

	var request agentprotocol.InitRequest
	if err := agentprotocol.ReadFrame(file, &request); err != nil {
		return fmt.Errorf("read initialization frame: %w", err)
	}

	_ = unix.SetsockoptTimeval(connFd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{})
	response := handler.HandleRequest(request)

	_ = unix.SetsockoptTimeval(connFd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
	if err := agentprotocol.WriteFrame(file, response); err != nil {
		return fmt.Errorf("write initialization response: %w", err)
	}

	if response.Status == "ready" {
		ulog.GetLogger().Info("vsock sent READY response", ulog.F("fd", connFd))
	}
	return nil
}
