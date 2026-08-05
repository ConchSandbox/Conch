package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/openeuler/Conch/internal/apierror"
	cmd "github.com/openeuler/Conch/internal/cli/cmd"
	"github.com/openeuler/Conch/internal/version"
)

func printHelp(out io.Writer) {
	fmt.Fprintln(out, "conch - Conch command-line tool")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch <command> [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Commands:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  image\tPull, push, unpack, list, or remove images.")
	fmt.Fprintln(tw, "  sandbox\tCreate, checkpoint, or control sandboxes from Template IDs.")
	fmt.Fprintln(tw, "  template\tBuild, list, inspect, or remove templates.")
	fmt.Fprintln(tw, "  debug\tLow-level inspection and repair commands.")
	_ = tw.Flush()
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Run 'conch <command> --help' for command-specific usage.")
}

func Run(args []string) int {
	if len(args) == 1 && (args[0] == "-v" || args[0] == "--version") {
		fmt.Fprintln(os.Stdout, version.String())
		return 0
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printHelp(os.Stdout)
		return 0
	}
	if len(args) < 1 {
		printHelp(os.Stderr)
		return 2
	}
	sub := args[0]
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var err error
	switch sub {
	case "image":
		err = cmd.RunImage(ctx, args[1:])
	case "sandbox":
		err = cmd.RunSandbox(ctx, args[1:])
	case "template":
		err = cmd.RunTemplate(ctx, args[1:])
	case "debug":
		err = cmd.RunDebug(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", sub)
		printHelp(os.Stderr)
		return 2
	}
	if err != nil {
		if errors.Is(err, apierror.ErrTemplateNotFound) {
			fmt.Fprintln(os.Stderr, "template not found")
			return 3
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
