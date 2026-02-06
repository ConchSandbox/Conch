package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "github.com/openeuler/Conch/api/go_proto"
)

const ServerPort = ":4064"

const ServerVersion = "0.0.1"

func main() {
	fmt.Println("This is conch-agent")
	fmt.Println("========================================")

	var chrootPath string
	flag.StringVar(&chrootPath, "path", "", "Path for chroot directory")
	flag.Parse()

	if chrootPath != "" {
		fmt.Printf("Executing chroot to: %s\n", chrootPath)

		if err := syscall.Chroot(chrootPath); err != nil {
			log.Fatalf("failed to chroot to %s: %v", chrootPath, err)
		}

		if err := os.Chdir("/"); err != nil {
			log.Fatalf("failed to chdir to /: %v", err)
		}

		fmt.Printf("Successfully changed root to %s\n", chrootPath)
	}

	listener, err := net.Listen("tcp", ServerPort)
	if err != nil {
		log.Fatalf("failed to create listener: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterAgentServiceServer(grpcServer, &AgentServer{Version: ServerVersion})

	reflection.Register(grpcServer)
	log.Printf("Agent gRPC server listening at %v (version: %s)", listener.Addr(), ServerVersion)

	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("failed to serve gRPC: %v", err)
	}

}
