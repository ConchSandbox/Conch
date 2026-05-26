package main

import (
	"os"

	"github.com/openeuler/Conch/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
