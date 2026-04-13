package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/openeuler/Conch/internal/image/conchbuild/buildahcli"
)

type pushOptions struct {
	localImage  string
	remoteImage string
	tlsVerify   bool
}

func printPushHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch push [options] <local-image> <remote-image>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Push a Conch OCI index with `buildah manifest push --all`.")
	fmt.Fprintln(out, "  Registry authentication is handled by buildah, for example `buildah login <registry>`.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --plain-http")
	fmt.Fprintln(out, "        allow plain HTTP / disable TLS verification for the destination registry")
	fmt.Fprintln(out, "  --tls-verify bool")
	fmt.Fprintln(out, "        verify TLS certificates for the destination registry (default: true)")
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

	cmdArgs := buildPushCommandArgs(opts)
	cmd := exec.CommandContext(ctx, buildahcli.Bin(), cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("conch push: buildah %s: %w", strings.Join(cmdArgs, " "), err)
	}

	fmt.Printf("Pushed image: %s -> %s\n", opts.localImage, opts.remoteImage)
	return nil
}

func parsePushArgs(args []string) (pushOptions, error) {
	opts := pushOptions{tlsVerify: true}
	images := make([]string, 0, 2)

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--plain-http":
			opts.tlsVerify = false
		case arg == "--tls-verify":
			if i+1 >= len(args) {
				return pushOptions{}, fmt.Errorf("conch push: missing value for %s", arg)
			}
			value, err := parseBoolFlagValue(args[i+1], arg)
			if err != nil {
				return pushOptions{}, err
			}
			opts.tlsVerify = value
			i++
		case strings.HasPrefix(arg, "--tls-verify="):
			value, err := parseBoolFlagValue(strings.TrimPrefix(arg, "--tls-verify="), "--tls-verify")
			if err != nil {
				return pushOptions{}, err
			}
			opts.tlsVerify = value
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

func parseBoolFlagValue(value, flagName string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes":
		return true, nil
	case "0", "f", "false", "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("conch push: invalid %s value %q", flagName, value)
	}
}

func buildPushCommandArgs(opts pushOptions) []string {
	args := []string{"manifest", "push", "--all"}
	if !opts.tlsVerify {
		args = append(args, "--tls-verify=false")
	}
	args = append(args, opts.localImage, ensureDockerTransport(opts.remoteImage))
	return args
}

func ensureDockerTransport(imageName string) string {
	if strings.Contains(imageName, "://") {
		return imageName
	}
	return "docker://" + imageName
}
