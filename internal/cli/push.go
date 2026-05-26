package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openeuler/Conch/internal/image/client"
)

type pushOptions struct {
	localImage  string
	remoteImage string
	plainHTTP   bool
	username    string
	password    string
	namespace   string
	configPath  string
}

func printPushHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch push [options] <local-image> <remote-image>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Push a Conch OCI index from conchd/containerd to a registry.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --plain-http")
	fmt.Fprintln(out, "        allow plain HTTP for the destination registry")
	fmt.Fprintln(out, "  --username string")
	fmt.Fprintln(out, "        registry username")
	fmt.Fprintln(out, "  --password string")
	fmt.Fprintln(out, "        registry password")
	fmt.Fprintln(out, "  -n, --namespace string")
	fmt.Fprintln(out, "        containerd namespace (default: config containerd.default_namespace or default)")
	fmt.Fprintln(out, "  --config string")
	fmt.Fprintln(out, "        config file path for conchd API discovery")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Example:")
	fmt.Fprintln(out, "  conch push localhost/demo-index:latest hub.oepkgs.net/conch/demo-index:latest")
	fmt.Fprintln(out, "  conch push --plain-http localhost/demo-index:latest conch.example.com/conch/demo-index:latest")
}

func runPush(ctx context.Context, args []string) error {
	opts, err := parsePushArgs(args)
	if err != nil {
		return err
	}
	cfg, err := loadConchConfig(opts.configPath)
	if err != nil {
		return fmt.Errorf("conch push: load config: %w", err)
	}
	ns := resolveConchNamespace(cfg, opts.namespace)
	conchClient := client.NewClientWithConfig("", opts.configPath)
	if err := conchClient.PushImage(ctx, client.PushImageRequest{
		LocalImage:  opts.localImage,
		RemoteImage: opts.remoteImage,
		Namespace:   ns,
		PlainHTTP:   opts.plainHTTP,
		Username:    opts.username,
		Password:    opts.password,
	}); err != nil {
		return fmt.Errorf("conch push: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Pushed image: %s -> %s\n", opts.localImage, opts.remoteImage)
	return nil
}

func parsePushArgs(args []string) (pushOptions, error) {
	opts := pushOptions{}
	images := make([]string, 0, 2)

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--plain-http":
			opts.plainHTTP = true
		case arg == "--username":
			if i+1 >= len(args) {
				return pushOptions{}, fmt.Errorf("conch push: missing value for %s", arg)
			}
			opts.username = args[i+1]
			i++
		case strings.HasPrefix(arg, "--username="):
			opts.username = strings.TrimPrefix(arg, "--username=")
		case arg == "--password":
			if i+1 >= len(args) {
				return pushOptions{}, fmt.Errorf("conch push: missing value for %s", arg)
			}
			opts.password = args[i+1]
			i++
		case strings.HasPrefix(arg, "--password="):
			opts.password = strings.TrimPrefix(arg, "--password=")
		case arg == "--namespace" || arg == "-n":
			if i+1 >= len(args) {
				return pushOptions{}, fmt.Errorf("conch push: missing value for %s", arg)
			}
			opts.namespace = args[i+1]
			i++
		case strings.HasPrefix(arg, "--namespace="):
			opts.namespace = strings.TrimPrefix(arg, "--namespace=")
		case strings.HasPrefix(arg, "-n="):
			opts.namespace = strings.TrimPrefix(arg, "-n=")
		case arg == "--config":
			if i+1 >= len(args) {
				return pushOptions{}, fmt.Errorf("conch push: missing value for %s", arg)
			}
			opts.configPath = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			opts.configPath = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-"):
			return pushOptions{}, fmt.Errorf("conch push: unknown option %s", arg)
		default:
			images = append(images, arg)
		}
	}

	if len(images) != 2 {
		return pushOptions{}, fmt.Errorf("conch push: exactly two image names are required: <local-image> <remote-image>")
	}
	opts.localImage = images[0]
	opts.remoteImage = images[1]
	return opts, nil
}
