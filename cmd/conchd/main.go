package main

import (
	"log"

	"github.com/openeuler/Conch/internal"
)

func main() {
	server, err := internal.NewServer()
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	defer server.Cleanup()

	log.Println("starting conchd server")
	if err := server.Start(":4063"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
