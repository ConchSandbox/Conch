package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/remotes/docker"

	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/pkg/ulog"
)

func printPullHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch pull [options] <image-name>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Pull a Conch native image into containerd content store, then unpack")
	fmt.Fprintln(out, "  all child images and link snapshot labels.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  -n, --namespace string")
	fmt.Fprintln(out, "        containerd namespace (default: config containerd.default_namespace or default)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintf(out, "        containerd socket address (default: config containerd.socket or %s)\n", defaultContainerdAddress)
	fmt.Fprintln(out, "  -config string")
	fmt.Fprintln(out, "        config file path (default: auto-detect common config paths)")
	fmt.Fprintln(out, "  --plain-http")
	fmt.Fprintln(out, "        allow plain HTTP / disable TLS verification for source image pulls")
	fmt.Fprintln(out, "  --user string")
	fmt.Fprintln(out, "        registry credentials in username:password format for source image pulls")
	fmt.Fprintln(out, "  --kernel-plain-http")
	fmt.Fprintln(out, "        allow plain HTTP / disable TLS verification for default kernel image pulls")
	fmt.Fprintln(out, "  --kernel-user string")
	fmt.Fprintln(out, "        registry credentials in username:password format for default kernel image pulls")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Example:")
	fmt.Fprintln(out, "  conch pull -n default hub.oepkgs.net/conch/sandbox-snapshot:latest")
	fmt.Fprintln(out, "  conch pull --kernel-plain-http --kernel-user example-user:example-password docker.io/library/nginx:latest")
}

func runPull(ctx context.Context, args []string) error {
	if err := initUnpackLogger(); err != nil {
		return err
	}
	defer func() {
		logger := ulog.GetLogger()
		if closer, ok := logger.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("address", "", "containerd socket address")
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	plainHTTP := fs.Bool("plain-http", false, "allow plain HTTP / disable TLS verification for source image pulls")
	user := fs.String("user", "", "registry credentials in username:password format for source image pulls")
	kernelPlainHTTP := fs.Bool("kernel-plain-http", false, "allow plain HTTP / disable TLS verification for default kernel image pulls")
	kernelUser := fs.String("kernel-user", "", "registry credentials in username:password format for default kernel image pulls")
	fs.StringVar(namespace, "n", "", "containerd namespace")
	fs.Usage = func() { printPullHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("conch pull: exactly one image name is required")
	}
	imageName := fs.Arg(0)

	cfg, err := loadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch pull: load config: %w", err)
	}
	containerdAddr, ns := resolveContainerdRuntime(cfg, *addr, *namespace)
	username, password, err := parseRegistryUser(*user)
	if err != nil {
		return fmt.Errorf("conch pull: %w", err)
	}
	kernelUsername, kernelPassword, err := parseRegistryUser(*kernelUser)
	if err != nil {
		return fmt.Errorf("conch pull: %w", err)
	}

	client, err := containerd.New(containerdAddr)
	if err != nil {
		return fmt.Errorf("connect to containerd: %w", err)
	}
	defer client.Close()

	pullCtx := namespaces.WithNamespace(ctx, ns)
	fmt.Println("------------------------------------------------------------")
	fmt.Printf("Pulling image: %s\n", imageName)
	resolver := docker.NewResolver(docker.ResolverOptions{
		PlainHTTP: *plainHTTP,
		Credentials: func(string) (string, string, error) {
			return username, password, nil
		},
	})
	pullOpts := []containerd.RemoteOpt{
		containerd.WithResolver(resolver),
	}
	if _, err := client.Pull(pullCtx, imageName, pullOpts...); err != nil {
		return fmt.Errorf("conch pull: pull image %s: %w", imageName, err)
	}

	if err := image.ValidateConchImageIndex(pullCtx, client, imageName); err != nil {
		results, convErr := image.PullAndUnpackOCIImage(pullCtx, client, image.PullOCIImageOptions{
			SourceImage:            imageName,
			DefaultKernelImage:     cfg.Image.DefaultKernelImage,
			SourcePlainHTTP:        *plainHTTP,
			SourceRegistryUsername: username,
			SourceRegistryPassword: password,
			KernelPlainHTTP:        *kernelPlainHTTP,
			KernelRegistryUsername: kernelUsername,
			KernelRegistryPassword: kernelPassword,
		})
		if convErr != nil {
			return fmt.Errorf("conch pull: image %s is not a supported Conch image and OCI conversion failed: %w", imageName, convErr)
		}
		printUnpackSummary(results)
		return nil
	}

	if _, err := client.Fetch(pullCtx, imageName, containerd.WithResolver(resolver)); err != nil {
		return fmt.Errorf("conch pull: fetch all Conch image content: %w", err)
	}

	results, err := image.UnpackAllSubImages(pullCtx, client, imageName)
	if err != nil {
		return fmt.Errorf("conch pull: unpack pulled image: %w", err)
	}
	printUnpackSummary(results)
	return nil
}

func parseRegistryUser(user string) (string, string, error) {
	if user == "" {
		return "", "", nil
	}
	idx := strings.IndexByte(user, ':')
	if idx <= 0 || idx == len(user)-1 {
		return "", "", fmt.Errorf("invalid --user value %q, want username:password", user)
	}
	return user[:idx], user[idx+1:], nil
}
