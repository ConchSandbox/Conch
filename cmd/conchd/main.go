package main

import (
	"fmt"
	"log"

	"github.com/openeuler/Conch/internal"
)

func main() {
	server, err := internal.NewServer()
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	defer server.Cleanup()	

	fmt.Println("Starting conchd server")
	if err := server.Start(":8080"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
