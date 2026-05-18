// conch push: Conch OCI indexes are pushed through buildah manifest push.
// conch unpack: boot OCI index components are unpacked into containerd and linked via snapshot labels.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/openeuler/Conch/pkg/ulog"
)

func printHelp(out io.Writer) {
	fmt.Fprintln(out, "conch - Conch image tool")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch push [options] <local-image> <remote-image>")
	fmt.Fprintln(out, "  conch pull [options] <image-name>")
	fmt.Fprintln(out, "  conch unpack [options] <image-name>")
	fmt.Fprintln(out, "  conch snapshot export [options]")
	fmt.Fprintln(out, "  conch --help")
	fmt.Fprintln(out, "  conch push --help")
	fmt.Fprintln(out, "  conch pull --help")
	fmt.Fprintln(out, "  conch unpack --help")
	fmt.Fprintln(out, "  conch snapshot --help")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  push    Push a Conch OCI index using `buildah manifest push --all`.")
	fmt.Fprintln(out, "  pull    Pull a Conch native image and unpack it into containerd snapshots.")
	fmt.Fprintln(out, "  unpack  Unpack a Conch boot OCI index into containerd snapshots.")
	fmt.Fprintln(out, "  snapshot Export a sandbox-snapshot image from an existing snapshot or sandbox.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Environment:")
	fmt.Fprintln(out, "  CONCHD_HOST/CONCHD_PORT  fallback for conchd API URL")
}

func initUnpackLogger() error {
	cfg := ulog.Config{
		Level:      ulog.InfoLevel,
		OutputPath: "/var/log/conch/",
		Stdout:     true,
	}
	if err := ulog.Init(cfg); err == nil {
		return nil
	}

	// Keep unpack usable for non-root/dev environments where /var/log is not writable.
	if err := ulog.Init(ulog.Config{
		Level:  ulog.InfoLevel,
		Stdout: true,
	}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "warning: falling back to stdout-only logging for conch unpack")
	return nil
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		printHelp(os.Stdout)
		return
	}
	if len(os.Args) < 2 {
		printHelp(os.Stderr)
		os.Exit(2)
	}
	sub := os.Args[1]
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var err error
	switch sub {
	case "pull":
		if len(os.Args) >= 3 && (os.Args[2] == "-h" || os.Args[2] == "--help") {
			printPullHelp(os.Stdout)
			return
		}
		err = runPull(ctx, os.Args[2:])
	case "push":
		if len(os.Args) >= 3 && (os.Args[2] == "-h" || os.Args[2] == "--help") {
			printPushHelp(os.Stdout)
			return
		}
		err = runPush(ctx, os.Args[2:])
	case "unpack":
		if len(os.Args) >= 3 && (os.Args[2] == "-h" || os.Args[2] == "--help") {
			printUnpackHelp(os.Stdout)
			return
		}
		err = runUnpack(ctx, os.Args[2:])
	case "snapshot":
		err = runSnapshot(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", sub)
		printHelp(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
