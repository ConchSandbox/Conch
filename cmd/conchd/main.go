package main

import (
	"log"

	"github.com/openeuler/Conch/internal"
)

const listenPort = "4063"

func main() {
	server, err := internal.NewServer()
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	defer server.Cleanup()

	log.Println("starting conchd server")
	if err := server.Start(listenPort); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
