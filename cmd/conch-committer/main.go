// Command conch-committer is a drop-in replacement for OpenSandbox's
// image-committer. See package committer for the Job contract it honours.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/openeuler/Conch/internal/committer"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	args, err := committer.ParseArgs(argv)
	if err != nil {
		return err
	}

	if args.Mode == committer.ModeUnpause {
		return committer.RunUnpause(args)
	}

	// OpenSandbox injects CONTAINERD_SOCKET; for Conch point it at conchd's socket
	// via the controller's --containerd-socket-path.
	socket := os.Getenv("CONTAINERD_SOCKET")
	if socket == "" {
		return fmt.Errorf("CONTAINERD_SOCKET env is required (point --containerd-socket-path at conchd's unix socket)")
	}

	cfg := committer.Config{
		SandboxID:              os.Getenv("CONCH_SANDBOX_ID"),
		PlainHTTP:              os.Getenv("SNAPSHOT_REGISTRY_INSECURE") == "true",
		Username:               os.Getenv("CONCH_REGISTRY_USERNAME"),
		Password:               os.Getenv("CONCH_REGISTRY_PASSWORD"),
		TerminationMessagePath: os.Getenv("CONCH_TERMINATION_MESSAGE_PATH"),
	}
	client := committer.NewUnixClient(socket)
	return committer.RunCommit(context.Background(), client, cfg, args)
}
