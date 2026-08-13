package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openeuler/Conch/internal/cli/client"
)

func PrintImagePullHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch image pull [options] <image-name>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Pull a standard OCI image into the containerd content store.")
	fmt.Fprintln(out, "  Conch does not unpack OCI images. Boot Indexes must be pulled with")
	fmt.Fprintln(out, "  `conch template pull`.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  -api-url string")
	fmt.Fprintln(out, "        conchd API base URL (default: conchd unix socket from config)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintln(out, "        deprecated alias for -api-url")
	fmt.Fprintln(out, "  -config string")
	fmt.Fprintln(out, "        config file path (default: auto-detect common config paths)")
	fmt.Fprintln(out, "  --plain-http")
	fmt.Fprintln(out, "        allow plain HTTP / disable TLS verification for source image pulls")
	fmt.Fprintln(out, "  --user string")
	fmt.Fprintln(out, "        registry credentials in username:password format for source image pulls")
	fmt.Fprintln(out, "Example:")
	fmt.Fprintln(out, "  conch image pull docker.io/library/nginx:latest")
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
	conchClient, err := client.New(client.Options{
		BaseURL:    ResolveConchAPIURL(*apiURL, *addr),
		ConfigPath: *configPath,
	})
	if err != nil {
		return fmt.Errorf("conch image pull: create API client: %w", err)
	}
	fmt.Println("------------------------------------------------------------")
	fmt.Printf("Pulling image: %s\n", imageName)
	if err := conchClient.PullImage(ctx, client.PullImageRequest{
		ImageName: imageName,
		PlainHTTP: *plainHTTP,
		Username:  username,
		Password:  password,
	}); err != nil {
		return fmt.Errorf("conch image pull: %w", err)
	}
	fmt.Printf("Pulled image: %s\n", imageName)
	return nil
}
