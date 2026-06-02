package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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
	fmt.Fprintln(out, "  conch image ls [options]")
	fmt.Fprintln(out, "  conch image rm [options] <image-name>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  ls  List images from conchd/containerd.")
	fmt.Fprintln(out, "  rm  Remove an image from conchd/containerd.")
}

func runImage(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printImageHelp(os.Stdout)
		return nil
	}
	switch args[0] {
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
	var filters stringSliceFlag
	fs.StringVar(namespace, "n", "", "containerd namespace")
	fs.Var(&filters, "filter", "containerd image filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("conch image ls: unexpected positional arguments: %v", fs.Args())
	}
	cfg, err := loadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch image ls: load config: %w", err)
	}
	images, err := client.NewClientWithConfig("", *configPath).ListImages(ctx, client.ListImagesRequest{
		Namespace: resolveConchNamespace(cfg, *namespace),
		Filters:   filters,
	})
	if err != nil {
		return fmt.Errorf("conch image ls: %w", err)
	}
	fmt.Printf("%-64s %-18s %s\n", "DIGEST", "TARGET_SIZE", "NAME")
	for _, image := range images {
		fmt.Printf("%-64s %-18d %s\n", image.TargetDigest, image.Size, image.Name)
	}
	return nil
}

func runImageRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	synchronous := fs.Bool("sync", false, "remove image resources synchronously")
	fs.StringVar(namespace, "n", "", "containerd namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch image rm: exactly one image name is required")
	}
	cfg, err := loadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch image rm: load config: %w", err)
	}
	imageName := fs.Arg(0)
	if err := client.NewClientWithConfig("", *configPath).RemoveImage(ctx, client.RemoveImageRequest{
		ImageName:   imageName,
		Namespace:   resolveConchNamespace(cfg, *namespace),
		Synchronous: *synchronous,
	}); err != nil {
		return fmt.Errorf("conch image rm: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Removed image: %s\n", imageName)
	return nil
}
