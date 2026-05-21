// conch convert: converts an existing OCI rootfs image plus kernel/initrd files into a Conch boot image.
// conch unpack: boot OCI index components are unpacked into containerd and linked via snapshot labels.
package cli

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
	fmt.Fprintln(out, "  conch convert [options]")
	fmt.Fprintln(out, "  conch push [options] <local-image> <remote-image>")
	fmt.Fprintln(out, "  conch pull [options] <image-name>")
	fmt.Fprintln(out, "  conch unpack [options] <image-name>")
	fmt.Fprintln(out, "  conch snapshot export [options]")
	fmt.Fprintln(out, "  conch --help")
	fmt.Fprintln(out, "  conch convert --help")
	fmt.Fprintln(out, "  conch push --help")
	fmt.Fprintln(out, "  conch pull --help")
	fmt.Fprintln(out, "  conch unpack --help")
	fmt.Fprintln(out, "  conch snapshot --help")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  convert Convert an existing OCI rootfs image plus kernel/initrd files")
	fmt.Fprintln(out, "          into a Conch native EROFS boot image.")
	fmt.Fprintln(out, "  push    Push a Conch OCI index from conchd/containerd.")
	fmt.Fprintln(out, "  pull    Pull a Conch native image and unpack it into containerd snapshots.")
	fmt.Fprintln(out, "  unpack  Unpack a Conch boot OCI index into containerd snapshots.")
	fmt.Fprintln(out, "  snapshot Export a sandbox-snapshot image from an existing snapshot or sandbox.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Environment:")
	fmt.Fprintln(out, "  CONCH_API_URL            conchd API base URL")
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

func Run(args []string) int {
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
	case "convert":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			printConvertHelp(os.Stdout)
			return 0
		}
		err = runConvert(ctx, args[1:])
	case "pull":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			printPullHelp(os.Stdout)
			return 0
		}
		err = runPull(ctx, args[1:])
	case "push":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			printPushHelp(os.Stdout)
			return 0
		}
		err = runPush(ctx, args[1:])
	case "unpack":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			printUnpackHelp(os.Stdout)
			return 0
		}
		err = runUnpack(ctx, args[1:])
	case "snapshot":
		err = runSnapshot(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", sub)
		printHelp(os.Stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
