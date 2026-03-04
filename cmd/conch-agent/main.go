package main

import (
	"flag"
	"net"
	"os"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/pkg/ulog"
)

const ServerPort = ":4064"

const ServerVersion = "0.0.1"

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

	logger := ulog.GetLogger()

	logger.Info("Starting conch-agent",
		ulog.F("version", ServerVersion),
		ulog.F("port", ServerPort),
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

	if err := grpcServer.Serve(listener); err != nil {
		logger.Fatal("Failed to serve gRPC",
			ulog.F("error", err),
		)
	}

}
