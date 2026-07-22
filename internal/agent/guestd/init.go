// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: PID 1 initialization logic for conch-init

package guestd

import (
	"errors"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

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
	setupNetwork()

	mergeReady := false
	if _, err := os.Stat(MergeTarget + "/usr"); err == nil {
		mergeReady = true
	}

	if mergeReady {
		prepareMergeRoot()
		bindMountToMerge()
		setupDevPts()
		setupMergeFileLogging()

		rootfsEntrypointExpected.Store(hasRootfsEntrypoint())
		if rootfsEntrypointExpected.Load() {
			ulog.GetLogger().Info("Rootfs conch entrypoint found; using rootfs service startup")
			startRootfsEntrypoint()
		} else {
			ulog.GetLogger().Info("Rootfs conch entrypoint not found; skipping rootfs service startup")
		}

		if err := chrootToMerge(); err != nil {
			ulog.GetLogger().Error("Failed to chroot into merge layer, aborting init")
			return
		}
	} else {
		ulog.GetLogger().Warn("Overlay rootfs not found", ulog.F("target", MergeTarget))
	}

	if err := startAgentAPIServerAsync(); err != nil {
		ulog.GetLogger().Error("agent API server failed to start, vsock will report NOT_READY",
			ulog.F("error", err),
		)
	}

	go startVsockServer()
	go reapChildren()
	go waitForRootfsServiceReadySignal()

	ulog.GetLogger().Info("Initialization complete")
	waitForSignal()
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

// waitForSignal waits for SIGTERM/SIGINT
func waitForSignal() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	ulog.GetLogger().Info("Received shutdown signal")
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

// startVsockServer starts the vsock server for host communication.
func startVsockServer() {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		ulog.GetLogger().Error("Failed to create vsock socket", ulog.F("error", err))
		return
	}
	defer unix.Close(fd)

	sa := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY,
		Port: uint32(vsockReadyPort),
	}

	if err := unix.Bind(fd, sa); err != nil {
		ulog.GetLogger().Error("Failed to bind vsock", ulog.F("error", err))
		return
	}

	if err := unix.Listen(fd, 5); err != nil {
		ulog.GetLogger().Error("Failed to listen on vsock", ulog.F("error", err))
		return
	}

	ulog.GetLogger().Info("vsock server listening", ulog.F("port", vsockReadyPort))

	handler := NewVsockHandler(ServerVersion, checkSandboxReady)
	for {
		connFd, _, err := unix.Accept(fd)
		logger := ulog.GetLogger()
		if err != nil {
			logger.Error("vsock accept error", ulog.F("error", err))
			continue
		}

		buf := make([]byte, 1024)
		n, err := unix.Read(connFd, buf)
		if err != nil {
			logger.Error("vsock read error",
				ulog.F("fd", connFd),
				ulog.F("error", err),
			)
			_ = unix.Close(connFd)
			continue
		}
		if n > 0 {
			message := string(buf[:n])
			response := handler.HandleMessage(message)
			if response != "" {
				if _, err := unix.Write(connFd, []byte(response)); err != nil {
					ulog.GetLogger().Error("vsock write response error",
						ulog.F("fd", connFd),
						ulog.F("error", err),
					)
				} else if strings.Contains(response, "READY:") {
					ulog.GetLogger().Info("vsock sent READY response", ulog.F("fd", connFd))
				}
			}
		}
		_ = unix.Close(connFd)
	}
}
