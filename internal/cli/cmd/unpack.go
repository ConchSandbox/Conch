package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openeuler/Conch/internal/image/client"
	"github.com/openeuler/Conch/pkg/ulog"
)

func PrintImageUnpackHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch image unpack [options] <image-name>")
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
	fmt.Fprintln(out, "  conch image unpack -n default hub.oepkgs.net/conch/conch-index:v0.1")
}

func RunImageUnpack(ctx context.Context, args []string) error {
	if err := InitUnpackLogger(); err != nil {
		return err
	}
	defer func() {
		logger := ulog.GetLogger()
		if closer, ok := logger.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	fs := flag.NewFlagSet("image unpack", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	apiURL := fs.String("api-url", "", "conchd API base URL")
	addr := fs.String("address", "", "deprecated alias for -api-url")
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	fs.StringVar(namespace, "n", "", "containerd namespace")
	fs.Usage = func() { PrintImageUnpackHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("conch image unpack: exactly one image name is required")
	}
	imageName := fs.Arg(0)

	cfg, err := LoadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch image unpack: load config: %w", err)
	}
	ns := ResolveConchNamespace(cfg, *namespace)

	conchClient := client.NewClientWithConfig(ResolveConchAPIURL(*apiURL, *addr), *configPath)
	fmt.Println("------------------------------------------------------------")
	results, err := conchClient.UnpackImage(ctx, client.UnpackImageRequest{
		ImageName: imageName,
		Namespace: ns,
	})
	if err != nil {
		return fmt.Errorf("conch image unpack: %w", err)
	}
	PrintUnpackSummary(results)
	return nil
}
