package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/pkg/ulog"
)

func printUnpackHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch unpack [options] <image-name>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  -n, --namespace string")
	fmt.Fprintln(out, "        containerd namespace (default: config containerd.default_namespace or default)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintf(out, "        containerd socket address (default: config containerd.socket or %s)\n", defaultContainerdAddress)
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
	addr := fs.String("address", "", "containerd socket address")
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
	containerdAddr, ns := resolveContainerdRuntime(cfg, *addr, *namespace)

	client, err := containerd.New(containerdAddr)
	if err != nil {
		return fmt.Errorf("connect to containerd: %w", err)
	}
	defer client.Close()

	unpackCtx := namespaces.WithNamespace(ctx, ns)
	fmt.Println("------------------------------------------------------------")
	results, err := image.UnpackAllSubImages(unpackCtx, client, imageName)
	if err != nil {
		return fmt.Errorf("conch unpack: %w", err)
	}
	printUnpackSummary(results)
	return nil
}
