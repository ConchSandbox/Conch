// conch build: KERNEL/INDEX/SNAP Dockerfile extensions are preprocessed and post-processed in internal/image.
// conch push: Conch OCI indexes are pushed through buildah manifest push.
// conch unpack: boot OCI index components are unpacked into containerd and linked via snapshot labels.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"go.podman.io/storage/pkg/reexec"

	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/pkg/ulog"
)

func printHelp(out io.Writer) {
	fmt.Fprintln(out, "conch - Conch image tool")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch build [buildah-bud-args...]")
	fmt.Fprintln(out, "  conch push [options] <local-image> <remote-image>")
	fmt.Fprintln(out, "  conch pull [options] <image-name>")
	fmt.Fprintln(out, "  conch unpack [options] <image-name>")
	fmt.Fprintln(out, "  conch snapshot export [options]")
	fmt.Fprintln(out, "  conch --help")
	fmt.Fprintln(out, "  conch build --help")
	fmt.Fprintln(out, "  conch push --help")
	fmt.Fprintln(out, "  conch pull --help")
	fmt.Fprintln(out, "  conch unpack --help")
	fmt.Fprintln(out, "  conch snapshot --help")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  build   Forward arguments to `buildah bud` and consume Dockerfile")
	fmt.Fprintln(out, "          extensions `KERNEL`, `INDEX`, and `SNAP` in the Conch pipeline.")
	fmt.Fprintln(out, "  push    Push a Conch OCI index using `buildah manifest push --all`.")
	fmt.Fprintln(out, "  pull    Pull a Conch native image and unpack it into containerd snapshots.")
	fmt.Fprintln(out, "  unpack  Unpack a Conch boot OCI index into containerd snapshots.")
	fmt.Fprintln(out, "  snapshot Export a sandbox-snapshot image from an existing snapshot or sandbox.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Environment:")
	fmt.Fprintln(out, "  CONCH_BUILDAH_BIN        buildah binary path (default: buildah)")
	fmt.Fprintln(out, "  BUILDAH_CONCH_API_URL    conchd API base URL")
	fmt.Fprintln(out, "  CONCHD_HOST/CONCHD_PORT  fallback for conchd API URL")
}

func printBuildHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch build [buildah-bud-args...]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Forward arguments to `buildah bud` and consume Dockerfile")
	fmt.Fprintln(out, "  extensions `KERNEL`, `INDEX`, and `SNAP` in the Conch pipeline.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  -config, --config string")
	fmt.Fprintln(out, "        config file path for Conch SNAP flow")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Environment:")
	fmt.Fprintln(out, "  CONCH_BUILDAH_BIN        buildah binary path (default: buildah)")
	fmt.Fprintln(out, "  BUILDAH_CONCH_API_URL    conchd API base URL")
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

func runBuild(ctx context.Context, args []string) error {
	configPath, forward, err := parseBuildConfigArg(args)
	if err != nil {
		return err
	}
	_, err = image.BuildWithConchExtensions(ctx, image.BuildRequest{
		BuildahArgs: forward,
		ConfigPath:  configPath,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("conch build: %w", err)
	}
	return nil
}

func parseBuildConfigArg(args []string) (string, []string, error) {
	forward := make([]string, 0, len(args))
	configPath := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config" || arg == "-config":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("conch build: missing value for %s", arg)
			}
			configPath = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			configPath = strings.TrimPrefix(arg, "--config=")
			if configPath == "" {
				return "", nil, fmt.Errorf("conch build: empty --config=")
			}
		case strings.HasPrefix(arg, "-config="):
			configPath = strings.TrimPrefix(arg, "-config=")
			if configPath == "" {
				return "", nil, fmt.Errorf("conch build: empty -config=")
			}
		default:
			forward = append(forward, arg)
		}
	}
	return configPath, forward, nil
}
func main() {
	if reexec.Init() {
		return
	}

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
	case "build":
		if len(os.Args) >= 3 && (os.Args[2] == "-h" || os.Args[2] == "--help") {
			printBuildHelp(os.Stdout)
			return
		}
		err = runBuild(ctx, os.Args[2:])
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
