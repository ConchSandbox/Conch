package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/openeuler/Conch/internal/cli/client"
)

type ImagePushOptions struct {
	LocalImage    string
	RemoteImage   string
	PlainHTTP     bool
	Username      string
	Password      string
	PasswordStdin bool
	ConfigPath    string
	Timeout       string
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
	fmt.Fprintln(out, "        registry password (visible to every local user via ps; prefer --password-stdin)")
	fmt.Fprintln(out, "  --password-stdin")
	fmt.Fprintln(out, "        read the registry password from stdin")
	fmt.Fprintln(out, "  --timeout duration")
	fmt.Fprintln(out, "        timeout for this push operation")
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
	username, password, err := registryCredentials("", opts.Username, opts.Password, opts.PasswordStdin)
	if err != nil {
		return fmt.Errorf("conch image push: %w", err)
	}
	conchClient, err := client.New(client.Options{ConfigPath: opts.ConfigPath, Timeout: apiTimeout})
	if err != nil {
		return fmt.Errorf("conch image push: create API client: %w", err)
	}
	if err := conchClient.PushImage(ctx, client.PushImageRequest{
		LocalImage:  opts.LocalImage,
		RemoteImage: opts.RemoteImage,
		PlainHTTP:   opts.PlainHTTP,
		Username:    username,
		Password:    password,
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
		case arg == "--password-stdin":
			opts.PasswordStdin = true
		case arg == "--timeout":
			if i+1 >= len(args) {
				return ImagePushOptions{}, fmt.Errorf("conch image push: missing value for %s", arg)
			}
			opts.Timeout = args[i+1]
			i++
		case strings.HasPrefix(arg, "--timeout="):
			opts.Timeout = strings.TrimPrefix(arg, "--timeout=")
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
