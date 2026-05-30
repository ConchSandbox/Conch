package guestd

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	ServerPort     = ":4064"
	ServerVersion  = "0.0.3"
	vsockReadyPort = 4065
)

var (
	currentSandboxID string
	rootLogger       ulog.Logger
	mu               sync.Mutex
	isSafe           = true
)

func initDefaultLogger(logFile string) ulog.Logger {
	config := ulog.Config{
		Level:  ulog.InfoLevel,
		Stdout: true,
	}
	if logFile != "" {
		config.OutputFile = logFile
	}
	if err := ulog.Init(config); err != nil {
		panic(err)
	}

	logger := ulog.GetLogger()
	if currentSandboxID != "" {
		logger = logger.With(ulog.F("sandboxId", currentSandboxID))
		ulog.SetLogger(logger)
	}
	rootLogger = logger
	return logger
}

func refreshSandboxLoggerFromCmdline() string {
	sandboxID := getSandboxIDFromCmdline()
	if sandboxID == "" {
		return ""
	}

	mu.Lock()
	currentSandboxID = sandboxID
	mu.Unlock()

	if rootLogger == nil {
		rootLogger = ulog.GetLogger()
	}
	rootLogger = rootLogger.ReplaceField("sandboxId", sandboxID)
	ulog.SetLogger(rootLogger)
	return sandboxID
}

func getSandboxIDFromCmdline() string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	for _, field := range strings.Fields(string(data)) {
		if strings.HasPrefix(field, "conch.sandbox_id=") {
			return strings.TrimPrefix(field, "conch.sandbox_id=")
		}
	}
	return ""
}

func createVsockListener(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, fmt.Errorf("failed to create vsock socket: %w", err)
	}

	sa := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY,
		Port: port,
	}

	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("failed to bind vsock socket to port %d: %w", port, err)
	}

	if err := unix.Listen(fd, 5); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("failed to listen on vsock port %d: %w", port, err)
	}

	return fd, nil
}

func acceptVsockConnection(fd int) (int, error) {
	nfd, _, err := unix.Accept(fd)
	if err != nil {
		return -1, fmt.Errorf("failed to accept vsock connection: %w", err)
	}
	return nfd, nil
}

func listenVsockLoop(handler VsockHandler) {
	logger := ulog.GetLogger()
	logger.Info("Starting vsock listener loop", ulog.F("port", vsockReadyPort))

	fd, err := createVsockListener(vsockReadyPort)
	if err != nil {
		logger.Error("Failed to create vsock listener", ulog.F("error", err))
		return
	}
	defer unix.Close(fd)

	logger.Info("Vsock listener started", ulog.F("port", vsockReadyPort))

	for {
		connFd, err := acceptVsockConnection(fd)
		if err != nil {
			logger.Error("Failed to accept vsock connection", ulog.F("error", err))
			continue
		}

		buf := make([]byte, 1024)
		n, err := unix.Read(connFd, buf)
		if err != nil {
			logger.Error("Failed to read from vsock connection", ulog.F("error", err))
		} else if n > 0 {
			message := string(buf[:n])
			logger.Info("Received message via vsock", ulog.F("message", message))

			response := handler.HandleMessage(message)
			if response != "" {
				unix.Write(connFd, []byte(response))
			}
		}
		logger = ulog.GetLogger()
		if err := unix.Close(connFd); err != nil {
			logger.Error("Failed to close vsock connection", ulog.F("error", err))
		}
		logger.Info("Continuing to listen for next signal via vsock")
	}
}

func Run(args []string) error {
	flags := flag.NewFlagSet("conch-agent", flag.ExitOnError)
	var asInit bool
	var chrootPath string
	flags.BoolVar(&asInit, "init", false, "Run as init process (PID 1)")
	flags.StringVar(&chrootPath, "path", "", "Path for chroot directory")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if !asInit && os.Getpid() == 1 {
		asInit = true
	}
	if asInit {
		initDefaultLogger("")
		runAsInit()
		return nil
	}

	// Initialize logger
	logDir := "/var/log/conch-agent/"
	err := os.MkdirAll(logDir, 0755)
	if err != nil {
		panic(err)
	}

	err = ulog.Init(ulog.Config{
		Level:      ulog.DebugLevel,
		OutputPath: logDir,
		Stdout:     true,
	})
	if err != nil {
		panic(err)
	}

	rootLogger = ulog.GetLogger()
	defer func() {
		if closer, ok := rootLogger.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	// Get initial sandbox ID from cmdline
	initialSandboxID := getSandboxIDFromCmdline()
	if initialSandboxID != "" {
		mu.Lock()
		currentSandboxID = initialSandboxID
		mu.Unlock()
	}

	ulog.SetLogger(rootLogger.With(ulog.F("sandboxId", currentSandboxID)))
	logger := ulog.GetLogger()

	logger.Info("Starting conch-agent",
		ulog.F("version", ServerVersion),
		ulog.F("server_port", ServerPort),
		ulog.F("vsock_port", vsockReadyPort),
	)

	if chrootPath != "" {
		logger.Info("Executing chroot", ulog.F("path", chrootPath))

		if err := syscall.Chroot(chrootPath); err != nil {
			logger.Fatal("Failed to chroot",
				ulog.F("path", chrootPath),
				ulog.F("error", err),
			)
		}

		if err := os.Chdir("/"); err != nil {
			logger.Fatal("Failed to chdir to /",
				ulog.F("error", err),
			)
		}

		logger.Info("Successfully changed root",
			ulog.F("path", chrootPath),
		)
	}

	listener, err := net.Listen("tcp", ServerPort)
	if err != nil {
		logger.Fatal("Failed to create listener",
			ulog.F("port", ServerPort),
			ulog.F("error", err),
		)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterAgentServiceServer(grpcServer, &AgentServer{Version: ServerVersion})

	reflection.Register(grpcServer)

	go func() {
		logger.Info("gRPC server starting", ulog.F("address", listener.Addr()))

		if err := grpcServer.Serve(listener); err != nil {
			mu.Lock()
			isSafe = false
			mu.Unlock()
			logger.Error("gRPC server failed", ulog.F("error", err))
		}
	}()

	vsockHandler := NewVsockHandler(ServerVersion, checkGRPCHealth)
	listenVsockLoop(vsockHandler)
	return nil
}
