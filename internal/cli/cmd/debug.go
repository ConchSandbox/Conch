package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
)

func printDebugHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch debug snapshot ls [options]")
	fmt.Fprintln(out, "  conch debug snapshot rm [options] <snapshot-key>")
}

func RunDebug(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printDebugHelp(os.Stdout)
		return nil
	}
	switch args[0] {
	case "snapshot":
		return runSnapshot(ctx, args[1:])
	default:
		printDebugHelp(os.Stderr)
		return fmt.Errorf("unknown debug command %q", args[0])
	}
}
