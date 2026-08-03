package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/openeuler/Conch/internal/image/client"
)

type ImagePushOptions struct {
	LocalImage  string
	RemoteImage string
	PlainHTTP   bool
	Username    string
	Password    string
	Namespace   string
	ConfigPath  string
	Timeout     string
}

func PrintImagePushHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch image push [options] <local-image> <remote-image>")
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
	fmt.Fprintln(out, "        registry password (omit to use an interactive no-echo prompt when authentication is required)")
	fmt.Fprintln(out, "  --timeout duration")
	fmt.Fprintln(out, "        timeout for this push operation")
	fmt.Fprintln(out, "  -n, --namespace string")
	fmt.Fprintln(out, "        containerd namespace (default: config containerd.default_namespace or default)")
	fmt.Fprintln(out, "  --config string")
	fmt.Fprintln(out, "        config file path for conchd API discovery")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Example:")
	fmt.Fprintln(out, "  conch image push localhost/demo-index:latest hub.oepkgs.net/conch/demo-index:latest")
	fmt.Fprintln(out, "  conch image push --timeout 30m localhost/demo-index:latest hub.oepkgs.net/conch/demo-index:latest")
	fmt.Fprintln(out, "  conch image push --plain-http localhost/demo-index:latest conch.example.com/conch/demo-index:latest")
}

func RunImagePush(ctx context.Context, args []string) error {
	opts, err := ParseImagePushArgs(args)
	if err != nil {
		return err
	}
	var apiTimeout time.Duration
	if opts.Timeout != "" {
		apiTimeout, err = time.ParseDuration(opts.Timeout)
		if err != nil || apiTimeout <= 0 {
			return fmt.Errorf("conch image push: invalid --timeout %q", opts.Timeout)
		}
	}
	cfg, err := LoadConchConfig(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("conch image push: load config: %w", err)
	}
	ns := ResolveConchNamespace(cfg, opts.Namespace)
	conchClient := client.NewClientWithConfigAndTimeout("", opts.ConfigPath, apiTimeout)
	if err := pushWithRegistryAuth(ctx, opts.RemoteImage, opts.Username, opts.Password, func(username, password string) error {
		return conchClient.PushImage(ctx, client.PushImageRequest{
			LocalImage:      opts.LocalImage,
			RemoteImage:     opts.RemoteImage,
			Namespace:       ns,
			PlainHTTP:       opts.PlainHTTP,
			Username:        username,
			Password:        password,
			RegistryTimeout: opts.Timeout,
		})
	}); err != nil {
		return fmt.Errorf("conch image push: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Pushed image: %s -> %s\n", opts.LocalImage, opts.RemoteImage)
	return nil
}

func ParseImagePushArgs(args []string) (ImagePushOptions, error) {
	opts := ImagePushOptions{}
	images := make([]string, 0, 2)

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--plain-http":
			opts.PlainHTTP = true
		case arg == "--username":
			if i+1 >= len(args) {
				return ImagePushOptions{}, fmt.Errorf("conch image push: missing value for %s", arg)
			}
			opts.Username = args[i+1]
			i++
		case strings.HasPrefix(arg, "--username="):
			opts.Username = strings.TrimPrefix(arg, "--username=")
		case arg == "--password":
			if i+1 >= len(args) {
				return ImagePushOptions{}, fmt.Errorf("conch image push: missing value for %s", arg)
			}
			opts.Password = args[i+1]
			i++
		case strings.HasPrefix(arg, "--password="):
			opts.Password = strings.TrimPrefix(arg, "--password=")
		case arg == "--timeout":
			if i+1 >= len(args) {
				return ImagePushOptions{}, fmt.Errorf("conch image push: missing value for %s", arg)
			}
			opts.Timeout = args[i+1]
			i++
		case strings.HasPrefix(arg, "--timeout="):
			opts.Timeout = strings.TrimPrefix(arg, "--timeout=")
		case arg == "--namespace" || arg == "-n":
			if i+1 >= len(args) {
				return ImagePushOptions{}, fmt.Errorf("conch image push: missing value for %s", arg)
			}
			opts.Namespace = args[i+1]
			i++
		case strings.HasPrefix(arg, "--namespace="):
			opts.Namespace = strings.TrimPrefix(arg, "--namespace=")
		case strings.HasPrefix(arg, "-n="):
			opts.Namespace = strings.TrimPrefix(arg, "-n=")
		case arg == "--config":
			if i+1 >= len(args) {
				return ImagePushOptions{}, fmt.Errorf("conch image push: missing value for %s", arg)
			}
			opts.ConfigPath = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			opts.ConfigPath = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-"):
			return ImagePushOptions{}, fmt.Errorf("conch image push: unknown option %s", arg)
		default:
			images = append(images, arg)
		}
	}

	if len(images) != 2 {
		return ImagePushOptions{}, fmt.Errorf("conch image push: exactly two image names are required: <local-image> <remote-image>")
	}
	opts.LocalImage = images[0]
	opts.RemoteImage = images[1]
	return opts, nil
}
