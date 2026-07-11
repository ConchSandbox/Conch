package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/openeuler/Conch/internal/image/client"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func printImageHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch image <command> [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  pull    Pull an image and optionally unpack it.")
	fmt.Fprintln(out, "  push    Push an image to a registry.")
	fmt.Fprintln(out, "  unpack  Unpack a local image into snapshots.")
	fmt.Fprintln(out, "  ls      List images from conchd/containerd.")
	fmt.Fprintln(out, "  rm      Remove an image from conchd/containerd.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Run 'conch image <command> --help' for command-specific usage.")
}

func RunImage(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printImageHelp(os.Stdout)
		return nil
	}
	switch args[0] {
	case "pull":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			PrintImagePullHelp(os.Stdout)
			return nil
		}
		return RunImagePull(ctx, args[1:])
	case "push":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			PrintImagePushHelp(os.Stdout)
			return nil
		}
		return RunImagePush(ctx, args[1:])
	case "unpack":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			PrintImageUnpackHelp(os.Stdout)
			return nil
		}
		return RunImageUnpack(ctx, args[1:])
	case "ls":
		return runImageList(ctx, args[1:])
	case "rm":
		return runImageRemove(ctx, args[1:])
	default:
		printImageHelp(os.Stderr)
		return fmt.Errorf("unknown image command %q", args[0])
	}
}

func runImageList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image ls", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	showAll := fs.Bool("all", false, "show internal containerd image records")
	var filters stringSliceFlag
	fs.StringVar(namespace, "n", "", "containerd namespace")
	fs.Var(&filters, "filter", "containerd image filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("conch image ls: unexpected positional arguments: %v", fs.Args())
	}
	cfg, err := LoadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch image ls: load config: %w", err)
	}
	images, err := client.NewClientWithConfig("", *configPath).ListImages(ctx, client.ListImagesRequest{
		Namespace: ResolveConchNamespace(cfg, *namespace),
		Filters:   filters,
	})
	if err != nil {
		return fmt.Errorf("conch image ls: %w", err)
	}
	return printImageList(os.Stdout, images, *showAll)
}

func printImageList(out io.Writer, images []client.ImageRecord, showAll bool) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tDIGEST\tSIZE")
	for _, image := range images {
		if !showAll && isInternalImageRecord(image) {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", image.Name, displayImageKind(image.Kind), image.TargetDigest, image.Size)
	}
	return tw.Flush()
}

func isInternalImageRecord(image client.ImageRecord) bool {
	switch strings.TrimSpace(image.Kind) {
	case "rootfs", "sandbox", "mem-snapshot":
		return true
	}
	name := strings.TrimSpace(image.Name)
	return strings.HasPrefix(name, "conch-erofs-rootfs:") ||
		strings.HasPrefix(name, "conch-kernel:")
}

func displayImageKind(kind string) string {
	if strings.TrimSpace(kind) == "" {
		return "-"
	}
	return kind
}

func runImageRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	synchronous := fs.Bool("sync", true, "delete the containerd image record synchronously")
	fs.StringVar(namespace, "n", "", "containerd namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch image rm: exactly one image name is required")
	}
	cfg, err := LoadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch image rm: load config: %w", err)
	}
	imageName := fs.Arg(0)
	if err := client.NewClientWithConfig("", *configPath).RemoveImage(ctx, client.RemoveImageRequest{
		ImageName:   imageName,
		Namespace:   ResolveConchNamespace(cfg, *namespace),
		Synchronous: *synchronous,
	}); err != nil {
		return fmt.Errorf("conch image rm: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Removed image: %s\n", imageName)
	return nil
}
