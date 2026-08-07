package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openeuler/Conch/internal/cli/client"
	"github.com/openeuler/Conch/pkg/ulog"
)

func PrintImagePullHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch image pull [options] <image-name>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Pull a Conch native image into containerd content store, then unpack")
	fmt.Fprintln(out, "  all child images and link snapshot labels.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
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
	fmt.Fprintln(out, "  --skip-unpack")
	fmt.Fprintln(out, "        pull image content without creating local snapshots")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Example:")
	fmt.Fprintln(out, "  conch image pull hub.oepkgs.net/conch/sandbox-snapshot:latest")
	fmt.Fprintln(out, "  conch image pull --skip-unpack docker.io/library/nginx:latest")
	fmt.Fprintln(out, "  conch image pull --plain-http --user example-user:example-password docker.io/library/nginx:latest")
}

func RunImagePull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image pull", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	apiURL := fs.String("api-url", "", "conchd API base URL")
	addr := fs.String("address", "", "deprecated alias for -api-url")
	configPath := fs.String("config", "", "config file path")
	plainHTTP := fs.Bool("plain-http", false, "allow plain HTTP / disable TLS verification for source image pulls")
	user := fs.String("user", "", "registry credentials in username:password format for source image pulls")
	skipUnpack := fs.Bool("skip-unpack", false, "pull image content without creating local snapshots")
	fs.Usage = func() { PrintImagePullHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("conch image pull: exactly one image name is required")
	}
	imageName := fs.Arg(0)

	username, password, err := ParseRegistryUser(*user)
	if err != nil {
		return fmt.Errorf("conch image pull: %w", err)
	}
	if !*skipUnpack {
		if err := InitUnpackLogger(); err != nil {
			return err
		}
		defer func() {
			logger := ulog.GetLogger()
			if closer, ok := logger.(interface{ Close() error }); ok {
				_ = closer.Close()
			}
		}()
	}

	conchClient, err := client.New(client.Options{
		BaseURL:    ResolveConchAPIURL(*apiURL, *addr),
		ConfigPath: *configPath,
	})
	if err != nil {
		return fmt.Errorf("conch image pull: create API client: %w", err)
	}
	fmt.Println("------------------------------------------------------------")
	fmt.Printf("Pulling image: %s\n", imageName)
	results, err := conchClient.PullImage(ctx, client.PullImageRequest{
		ImageName:  imageName,
		PlainHTTP:  *plainHTTP,
		Username:   username,
		Password:   password,
		SkipUnpack: *skipUnpack,
	})
	if err != nil {
		return fmt.Errorf("conch image pull: %w", err)
	}
	if *skipUnpack {
		fmt.Printf("Pulled image without unpacking: %s\n", imageName)
		return nil
	}
	PrintUnpackSummary(results)
	return nil
}
