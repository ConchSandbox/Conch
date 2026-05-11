package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openeuler/Conch/internal/image/conchbuild/client"
	"github.com/openeuler/Conch/pkg/ulog"
)

func printUnpackHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch unpack [options] <image-name>")
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
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Example:")
	fmt.Fprintln(out, "  conch unpack -n default hub.oepkgs.net/conch/conch-index:v0.1")
}

func runUnpack(ctx context.Context, args []string) error {
	if err := initUnpackLogger(); err != nil {
		return err
	}
	defer func() {
		logger := ulog.GetLogger()
		if closer, ok := logger.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	fs := flag.NewFlagSet("unpack", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	apiURL := fs.String("api-url", "", "conchd API base URL")
	addr := fs.String("address", "", "deprecated alias for -api-url")
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	fs.StringVar(namespace, "n", "", "containerd namespace")
	fs.Usage = func() { printUnpackHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("conch unpack: exactly one image name is required")
	}
	imageName := fs.Arg(0)

	cfg, err := loadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch unpack: load config: %w", err)
	}
	ns := resolveConchNamespace(cfg, *namespace)

	conchClient := client.NewClientWithConfig(resolveConchAPIURL(*apiURL, *addr), *configPath)
	fmt.Println("------------------------------------------------------------")
	results, err := conchClient.UnpackImage(ctx, client.UnpackImageRequest{
		ImageName: imageName,
		Namespace: ns,
	})
	if err != nil {
		return fmt.Errorf("conch unpack: %w", err)
	}
	printUnpackSummary(results)
	return nil
}
