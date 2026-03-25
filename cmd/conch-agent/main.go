package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	ServerPort = ":4064"
	ServerVersion = "0.0.1"
	vsockCIDHost      = unix.VMADDR_CID_HOST  // CID 2 = host
	vsockReadyPort    = 4065                  // must match conchd side
	vsockReadyTimeout = 30 * time.Second      // timeout for ready signal loop
	vsockRetryBase    = 50 * time.Millisecond // base retry interval
	vsockRetryJitter  = 25 * time.Millisecond // max jitter (+/- 25ms)
)

var (
	currentSandboxID string
	agentLogger      ulog.Logger
	mu               sync.Mutex
)

// updateSandboxID reads sandbox_id from cmdline and updates global logger context
func updateSandboxID() {
	mu.Lock()
	defer mu.Unlock()

	id := getSandboxIDFromCmdline()
	if id == "" {
		return
	}

	// Always update if it's the first time or if it changed
	if id == currentSandboxID {
		return
	}

	currentSandboxID = id
	// Update the global logger with the new sandboxId field.
	// ulog.With returns a new logger instance with the added field.
	agentLogger = ulog.With(ulog.F("sandboxId", id))
	ulog.SetLogger(agentLogger)

	agentLogger.Info("Updated sandbox_id from cmdline", ulog.F("sandbox_id", id))
}

// monitorTimeDrift detects snapshot resume by monitoring system clock jumps
func monitorTimeDrift() {
	lastTick := time.Now()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		<-ticker.C
		now := time.Now()
		elapsed := now.Sub(lastTick)

		// If elapsed time since last tick is significantly larger than expected interval (500ms),
		// it indicates the VM was likely frozen/resumed.
		if elapsed > 2*time.Second {
			ulog.Info("Snapshot boot or significant time jump detected", ulog.F("elapsed", elapsed))
			updateSandboxID()
		}
		lastTick = now
	}
}

// getSandboxIDFromCmdline reads conch.sandbox_id from /proc/cmdline
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

// vsockDial connects to the host via AF_VSOCK using unix package
func vsockDial(cid uint32, port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}

	sa := &unix.SockaddrVM{
		CID:  cid,
		Port: port,
	}

	err = unix.Connect(fd, sa)
	if err != nil {
		unix.Close(fd)
		return -1, err
	}

	return fd, nil
}

// sendReadySignalLoop sends ready signal to host via vsock.
// Runs a loop with 50ms interval until ACK received or timeout:
//   - Fresh create: signals within ~50ms after gRPC is bound.
//   - Snapshot restore: goroutine resumes from freeze, signals within ~50ms.
//
// After wait for ACK from conchd, the listener socket is removed.
// Subsequent vsockDial attempts fail immediately (ECONNREFUSED in kernel),
// costing ~20 failed syscalls/second — negligible overhead, zero network traffic.
func sendReadySignalLoop(logger ulog.Logger, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			logger.Warn("vsock ready signal loop timed out", ulog.F("timeout", timeout))
			return
		}

		fd, err := vsockDial(vsockCIDHost, vsockReadyPort)
		if err == nil {
			logger.Info("Sent agent readiness signal via vsock")

			// Send the signal payload
			unix.Write(fd, []byte("READY\n"))

			// Wait for ACK from conchd. When received, it means the host has
			// successfully recorded our readiness and we can stop the loop.
			buf := make([]byte, 8)
			n, _ := unix.Read(fd, buf)
			if n > 0 && string(buf[:n]) == "ACK" {
				logger.Info("Received ACK from host, stopping vsock signal loop")
				unix.Close(fd)
				return
			}
			unix.Close(fd)
		}
		jitter := time.Duration(rand.Int63n(int64(vsockRetryJitter)*2)) - vsockRetryJitter
		time.Sleep(vsockRetryBase + jitter)
	}
}

func main() {
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
	defer func() {
		logger := ulog.GetLogger()
		if closer, ok := logger.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	updateSandboxID()
	logger := ulog.GetLogger()

	logger.Info("Starting conch-agent",
		ulog.F("version", ServerVersion),
		ulog.F("server_port", ServerPort),
		ulog.F("vsock_port", vsockReadyPort),
	)

	var chrootPath string
	flag.StringVar(&chrootPath, "path", "", "Path for chroot directory")
	flag.Parse()

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
	logger.Info("Agent gRPC server listening",
		ulog.F("address", listener.Addr()),
		ulog.F("version", ServerVersion),
	)

	// Start vsock ready signal loop AFTER gRPC is bound
	go sendReadySignalLoop(logger, vsockReadyTimeout)

	// Start snapshot boot monitor
	go monitorTimeDrift()

	if err := grpcServer.Serve(listener); err != nil {
		logger.Fatal("Failed to serve gRPC",
			ulog.F("error", err),
		)
	}

}
