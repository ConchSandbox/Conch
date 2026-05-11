package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openeuler/Conch/internal/image/conchbuild/client"
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
	fmt.Fprintln(out, "  -api-url string")
	fmt.Fprintln(out, "        conchd API base URL (default: config server endpoint or http://localhost:4063)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintln(out, "        deprecated alias for -api-url")
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
	apiURL := fs.String("api-url", "", "conchd API base URL")
	addr := fs.String("address", "", "deprecated alias for -api-url")
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
	ns := resolveConchNamespace(cfg, *namespace)
	username, password, err := parseRegistryUser(*user)
	if err != nil {
		return fmt.Errorf("conch pull: %w", err)
	}
	kernelUsername, kernelPassword, err := parseRegistryUser(*kernelUser)
	if err != nil {
		return fmt.Errorf("conch pull: %w", err)
	}

	conchClient := client.NewClientWithConfig(resolveConchAPIURL(*apiURL, *addr), *configPath)
	fmt.Println("------------------------------------------------------------")
	fmt.Printf("Pulling image: %s\n", imageName)
	results, err := conchClient.PullImage(ctx, client.PullImageRequest{
		ImageName:          imageName,
		Namespace:          ns,
		PlainHTTP:          *plainHTTP,
		Username:           username,
		Password:           password,
		DefaultKernelImage: cfg.Image.DefaultKernelImage,
		KernelPlainHTTP:    *kernelPlainHTTP,
		KernelUsername:     kernelUsername,
		KernelPassword:     kernelPassword,
	})
	if err != nil {
		return fmt.Errorf("conch pull: %w", err)
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
