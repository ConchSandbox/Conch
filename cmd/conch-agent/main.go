package main

import (
	"fmt"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/openeuler/Conch/cmd/conch-agent/pb"
)

const ServerPort = ":4064"

const ServerVersion = "0.0.1"

func main() {
	fmt.Println("This is conch-agent")
	fmt.Println("========================================")

	if len(os.Args) > 1 {
		fmt.Printf("arguments: %v\n", os.Args[1:])
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
