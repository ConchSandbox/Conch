package main

import (
	"fmt"
	"os"

	"github.com/openeuler/Conch/internal/agent/guestd"
)

func main() {
	if err := guestd.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "conch-init failed: %v\n", err)
		os.Exit(1)
	}
}
